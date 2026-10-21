// Command nu is the Nu CLI and daemon.
//
// The CLI talks to a node only through the node.Node interface, implemented over HTTP by
// the client package and in-process by node.Local for daemonless commands. Every command accepts
// --json; errors print their message and action and exit with the code's category
// (apiv0.ExitCode).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/swarm"
)

// The CLI is built only on the client package; this pins the seam.
var _ node.Node = (*client.Client)(nil)

// command is one (sub)command. Leaf commands have run; groups have sub.
type command struct {
	name    string
	args    string // usage after the name, e.g. "<key> <value>"
	summary string
	run     func(ctx context.Context, e *env, args []string) error
	sub     []*command
}

func (c *command) lookup(name string) *command {
	for _, s := range c.sub {
		if s.name == name {
			return s
		}
	}
	return nil
}

// env is the per-invocation environment.
type env struct {
	stdout, stderr io.Writer
	json           bool
	nuPath         string // --nu-path override
	apiAddr        string // --api override
	repo           *repo.Repo
	network        bool     // start the swarm in an in-process node
	verbose        bool     // --verbose: log in-process networking to stderr
	path           []string // command path, for usage
	cleanup        []func() // run after the command (in-process node, repo lock)
}

func (e *env) close() {
	for i := len(e.cleanup) - 1; i >= 0; i-- {
		e.cleanup[i]()
	}
	e.cleanup = nil
}

// usageError is a usage problem: exits with ExitUsage after printing the message and usage.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

func main() {
	// quic-go logs once when the UDP buffers cannot be enlarged; that is tuning advice,
	// not a problem to show on every command (`nu doctor` reports it).
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runCtx(ctx, args, stdout, stderr)
}

// runCtx runs one invocation; ctx ending acts like Ctrl-C.
func runCtx(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr}
	defer e.close()

	gfs := flag.NewFlagSet(nu.Name, flag.ContinueOnError)
	gfs.SetOutput(io.Discard)
	e.globalFlags(gfs)
	if err := gfs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, root, nil)
			return apiv0.ExitOK
		}
		return e.fail(usagef("%v", err), root)
	}
	rest := gfs.Args()
	if len(rest) == 0 {
		printUsage(stdout, root, nil)
		return apiv0.ExitUsage
	}
	cmd, rest := root, rest
	for len(rest) > 0 && cmd.sub != nil {
		if strings.HasPrefix(rest[0], "-") && cmd.run != nil {
			break // a group with a default action, given flags
		}
		next := cmd.lookup(rest[0])
		if next == nil {
			if rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
				printUsage(stdout, cmd, e.path)
				return apiv0.ExitOK
			}
			return e.fail(usagef("unknown command %q", strings.Join(append(e.path, rest[0]), " ")), cmd)
		}
		cmd, rest = next, rest[1:]
		e.path = append(e.path, cmd.name)
	}
	if cmd.run == nil {
		printUsage(stdout, cmd, e.path)
		return apiv0.ExitUsage
	}
	err := cmd.run(ctx, e, rest)
	if err == nil {
		return apiv0.ExitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		printUsage(stdout, cmd, e.path)
		return apiv0.ExitOK
	}
	return e.fail(err, cmd)
}

func (e *env) globalFlags(fs *flag.FlagSet) {
	fs.BoolVar(&e.json, "json", false, "machine-readable JSON output")
	fs.StringVar(&e.nuPath, "nu-path", "", "repo directory (default $"+repo.EnvPath+" or ~/"+nu.DefaultDir+")")
	fs.StringVar(&e.apiAddr, "api", "", "daemon API address (default: the running daemon's)")
	fs.BoolVar(&e.verbose, "verbose", false, "log in-process networking to stderr")
}

// flags returns a FlagSet for a leaf command with --json registered, so the flag may
// appear before or after the command name.
func (e *env) flags() *flag.FlagSet {
	fs := flag.NewFlagSet(strings.Join(e.path, " "), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&e.json, "json", e.json, "machine-readable JSON output")
	return fs
}

// parseFlags parses flags that may appear before or after positional arguments
// (`nu get <link> -o out` and `nu get -o out <link>` both work); fs.Args() returns the
// positional arguments in order. A literal `--` ends flag parsing.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return usagef("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			positional = append(positional, rest...)
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return fs.Parse(append([]string{"--"}, positional...))
}

// fail prints an error in the command's output mode and returns the exit code.
func (e *env) fail(err error, cmd *command) int {
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(e.stderr, "%s: %s\n\n", nu.Name, ue.msg)
		printUsage(e.stderr, cmd, e.path)
		return apiv0.ExitUsage
	}
	ae := toAPIError(err)
	if e.json {
		b, _ := json.Marshal(apiv0.Envelope{Error: ae})
		fmt.Fprintln(e.stderr, string(b))
	} else {
		fmt.Fprintf(e.stderr, "%s: %s\n", nu.Name, ae.Message)
		if ae.Action != "" {
			fmt.Fprintf(e.stderr, "  → %s\n", ae.Action)
		}
	}
	return apiv0.ExitCode(ae.Code)
}

// toAPIError normalizes any error to the envelope shape.
func toAPIError(err error) *apiv0.Error {
	var ae *apiv0.Error
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, context.Canceled):
		return &apiv0.Error{Code: apiv0.CodeCancelled, Message: "interrupted"}
	case errors.Is(err, context.DeadlineExceeded):
		return &apiv0.Error{Code: apiv0.CodeTimeout, Message: "timed out"}
	}
	return &apiv0.Error{Code: apiv0.CodeFailed, Message: err.Error()}
}

// openRepo opens (auto-initializing) the repo once per invocation.
func (e *env) openRepo() (*repo.Repo, error) {
	if e.repo != nil {
		return e.repo, nil
	}
	path := e.nuPath
	if path == "" {
		var err error
		if path, err = repo.DefaultPath(); err != nil {
			return nil, err
		}
	}
	r, created, err := repo.Open(path)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open %s: %v", path, err).
			WithAction("check that the directory is writable, or point " + repo.EnvPath + " elsewhere")
	}
	if created && !e.json {
		fmt.Fprintf(e.stderr, "%s: initialized %s\n", nu.Name, r.Root())
	}
	e.repo = r
	return r, nil
}

// node returns the daemon found through the repo (or --api). Commands that need the
// daemon (operations) use it; they fail with unavailable when none runs.
func (e *env) node(ctx context.Context) (node.Node, error) {
	return e.connect(ctx, "")
}

// nodeOrLocal is node for get/cat/ls/share: when no daemon runs, it starts an in-process
// node with networking for the duration of the command, holding the repo lock.
func (e *env) nodeOrLocal(ctx context.Context) (node.Node, error) {
	e.network = true
	return e.connect(ctx, "using an in-process node for this command")
}

// nodeOrLocalWrite is nodeOrLocal for add/pin/dag: the content lands in the store now
// (no networking) and is served once a node runs.
func (e *env) nodeOrLocalWrite(ctx context.Context) (node.Node, error) {
	e.network = false
	return e.connect(ctx, "working in-process; the content will be shared when a node runs (`"+nu.Name+" daemon` or `"+nu.Name+" share`)")
}

// connect finds the daemon; with a non-empty localNote it falls back to an in-process
// node, printing the note on stderr.
func (e *env) connect(ctx context.Context, localNote string) (node.Node, error) {
	if e.apiAddr != "" {
		return client.New(e.apiAddr), nil
	}
	r, err := e.openRepo()
	if err != nil {
		return nil, err
	}
	c, derr := client.Discover(r)
	if localNote == "" {
		return c, derr
	}
	if derr == nil {
		if _, err := c.Status(ctx); err == nil {
			return c, nil
		} else if ctx.Err() != nil {
			return nil, err
		}
	}
	n, release, err := e.local(r, e.network)
	if err != nil {
		return nil, err
	}
	e.cleanup = append(e.cleanup, release)
	if !e.json {
		fmt.Fprintf(e.stderr, "%s: no daemon running; %s\n", nu.Name, localNote)
	}
	return n, nil
}

// local opens an in-process node under the repo lock, with the swarm started when
// network is set (on an ephemeral port if the configured one is taken). The returned
// release closes the node and releases the lock.
func (e *env) local(r *repo.Repo, network bool) (*node.Local, func(), error) {
	// Take the repo lock to be sure no daemon is alive.
	lock, err := r.Lock()
	if err != nil {
		if errors.Is(err, repo.ErrLocked) {
			return nil, nil, apiv0.Errorf(apiv0.CodeUnavailable, "%v, but its API is not answering", err).
				WithAction("wait for it to finish starting, or stop it and run `" + nu.Name + " daemon` again")
		}
		return nil, nil, err
	}
	// Stale address files from a crashed daemon would mislead the next command.
	_ = r.RemoveAddrFile(repo.APIFile)
	_ = r.RemoveAddrFile(repo.GatewayFile)
	n, err := node.NewLocal(r)
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	if !network {
		return n, func() { n.Close(); lock.Release() }, nil
	}
	cfg, err := swarm.RepoConfig(r, n.Key, n.Store)
	if err != nil {
		n.Close()
		lock.Release()
		return nil, nil, err
	}
	cfg.Ephemeral = true
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if e.verbose {
		cfg.Logger = slog.New(slog.NewTextHandler(e.stderr, nil))
	}
	cfg.AllowPeer = n.GateSwarm(cfg.AllowPeer)
	if n.PrivateNetwork() {
		cfg.MDNS = false
	}
	sw, err := swarm.Start(context.Background(), cfg)
	if err != nil {
		n.Close()
		lock.Release()
		return nil, nil, err
	}
	n.Fetcher, n.Net = sw, sw
	if err := n.AttachNetwork(sw); err != nil {
		sw.Close()
		n.Close()
		lock.Release()
		return nil, nil, err
	}
	return n, func() {
		n.Ops.CancelAll()
		n.Wait()
		n.DetachNetwork()
		sw.Close()
		n.Close()
		lock.Release()
	}, nil
}

// emit writes v as JSON in --json mode, else calls text.
func (e *env) emit(v any, text func(w io.Writer)) error {
	if e.json {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	text(e.stdout)
	return nil
}

func printUsage(w io.Writer, cmd *command, path []string) {
	name := strings.Join(append([]string{nu.Name}, path...), " ")
	if cmd.sub != nil {
		fmt.Fprintf(w, "usage: %s <command> [args]\n\n", name)
		if cmd.summary != "" {
			fmt.Fprintf(w, "%s\n\n", cmd.summary)
		}
		fmt.Fprintln(w, "commands:")
		width := 0
		for _, s := range cmd.sub {
			width = max(width, len(s.name))
		}
		for _, s := range cmd.sub {
			fmt.Fprintf(w, "  %-*s  %s\n", width, s.name, s.summary)
		}
	} else {
		fmt.Fprintf(w, "usage: %s %s\n\n%s\n", name, cmd.args, cmd.summary)
	}
	if cmd == root {
		fmt.Fprintf(w, "\nglobal flags: --json, --nu-path <dir>, --api <host:port>, --verbose\n")
		fmt.Fprintf(w, "exit codes: %s\n", exitCodesSummary())
	}
}

func exitCodesSummary() string {
	codes := map[int]string{
		apiv0.ExitOK: "ok", apiv0.ExitFailed: "failed", apiv0.ExitUsage: "usage",
		apiv0.ExitNotFound: "not found", apiv0.ExitNotSupported: "not supported",
		apiv0.ExitConflict: "conflict", apiv0.ExitTimeout: "timeout",
		apiv0.ExitUnavailable: "daemon unavailable", apiv0.ExitForbidden: "forbidden",
		apiv0.ExitCancelled: "interrupted", apiv0.ExitNotImplemented: "not implemented",
	}
	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", k, codes[k]))
	}
	return strings.Join(parts, ", ")
}

var root = &command{
	summary: nu.Name + ": add files, share a link, fetch them elsewhere, browse them locally.",
	sub: []*command{
		cmdInit, cmdDaemon, cmdStatus, cmdDoctor, cmdConfig, cmdOperations,
		cmdAdd, cmdCat, cmdLs, cmdGet, cmdShare, cmdPin, cmdDag, cmdCar, cmdDataset, cmdCluster, cmdRelay, cmdVersion,
	},
}
