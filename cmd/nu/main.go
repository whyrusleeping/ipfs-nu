// Command nu is the Nu CLI and daemon.
//
// The CLI talks to a node only through the node.Node interface, implemented over HTTP by
// the client package (and later in-process for daemonless commands). Every command accepts
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
	path           []string // command path, for usage
}

// usageError is a usage problem: exits with ExitUsage after printing the message and usage.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
}

// flags returns a FlagSet for a leaf command with --json registered, so the flag may
// appear before or after the command name.
func (e *env) flags() *flag.FlagSet {
	fs := flag.NewFlagSet(strings.Join(e.path, " "), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&e.json, "json", e.json, "machine-readable JSON output")
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usagef("%v", err)
	}
	return nil
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

// node returns the node to talk to: the daemon found through the repo (or --api).
// Daemonless commands will fall back to an in-process node here.
func (e *env) node(ctx context.Context) (node.Node, error) {
	if e.apiAddr != "" {
		return client.New(e.apiAddr), nil
	}
	r, err := e.openRepo()
	if err != nil {
		return nil, err
	}
	return client.Discover(r)
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
		fmt.Fprintf(w, "\nglobal flags: --json, --nu-path <dir>, --api <host:port>\n")
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
		cmdAdd, cmdCat, cmdLs, cmdGet, cmdPin, cmdDag, cmdVersion,
	},
}
