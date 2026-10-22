//go:build linux && natlab

// Package natlab builds isolated Linux network topologies for opt-in tests.
// Run tests through Isolate before New: no command may touch the real network.
package natlab

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const isolatedEnv = "NU_NATLAB_ISOLATED"

// Isolate re-execs just this test in a fresh user+network namespace. The child
// returns true; the parent waits for it and returns false. Root uses a fresh
// network namespace too. Missing privileges/tools are failures, never skips.
// Each test must call this before creating a Lab. Use top-level tests only.
func Isolate(t *testing.T) bool {
	t.Helper()
	return IsolateTimeout(t, 45*time.Second)
}

// IsolateTimeout is Isolate with an explicit child deadline for opt-in bulk/soak
// scenarios. The default fast fixture deadline remains unchanged.
func IsolateTimeout(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	if timeout <= 0 || timeout > time.Hour {
		t.Fatal("natlab: invalid isolation timeout")
	}
	if os.Getenv(isolatedEnv) == "1" {
		return true
	}
	for _, tool := range []string{"unshare", "nsenter", "ip", "nft"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--net"}
	if os.Geteuid() != 0 {
		args = append(args, "--user", "--map-root-user")
	}
	args = append(args, "--", os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v", "-test.count=1", "-test.timeout="+timeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", args...)
	cmd.Env = append(os.Environ(), isolatedEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("isolated test (rootless=%t):\n%s", os.Geteuid() != 0, out)
	if err != nil {
		t.Fatalf("natlab isolation/test failed (enable user namespaces or run with root): %v", err)
	}
	return false
}

// Lab owns all processes, links and namespaces. It uses anonymous namespaces
// pinned by processes, not /run/netns mounts, so no mount privilege is needed.
// Close stops processes in reverse order and thereby destroys the namespaces.
// A Lab is a sequential scenario builder, not safe for concurrent mutation.
type Lab struct {
	ctx      context.Context
	cleanups []func()
	serial   int
}

func New(ctx context.Context) (*Lab, error) {
	if os.Getenv(isolatedEnv) != "1" {
		return nil, fmt.Errorf("natlab: call Isolate first")
	}
	return &Lab{ctx: ctx}, nil
}
func (l *Lab) Close() {
	for i := len(l.cleanups) - 1; i >= 0; i-- {
		l.cleanups[i]()
	}
	l.cleanups = nil
}

// Namespace is a process-pinned network namespace. Command supports arbitrary
// helper binaries (including third-party libp2p nodes) without touching Go threads.
type Namespace struct {
	lab  *Lab
	pid  int
	Name string
}

func (l *Lab) Namespace(name string) (*Namespace, error) {
	cmd := exec.CommandContext(l.ctx, "unshare", "--net", "--", "sh", "-c", "echo ready; exec cat >/dev/null")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		in.Close()
		return nil, err
	}
	l.cleanups = append(l.cleanups, func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		return nil, fmt.Errorf("namespace %s: %q: %w", name, line, err)
	}
	n := &Namespace{lab: l, pid: cmd.Process.Pid, Name: name}
	if _, err = n.Run("ip", "link", "set", "lo", "up"); err != nil {
		return nil, err
	}
	return n, nil
}
func (n *Namespace) Command(program string, args ...string) *exec.Cmd {
	prefix := []string{"--net=/proc/" + strconv.Itoa(n.pid) + "/ns/net", "--", program}
	return exec.CommandContext(n.lab.ctx, "nsenter", append(prefix, args...)...)
}
func (n *Namespace) Run(program string, args ...string) ([]byte, error) {
	out, err := n.Command(program, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %s %v: %w: %s", n.Name, program, args, err, out)
	}
	return out, nil
}
func (n *Namespace) NFT(rules string) error {
	cmd := n.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s nft: %w: %s\nrules:\n%s", n.Name, err, out, rules)
	}
	return nil
}

// Segment is an Ethernet bridge in a namespace. Put the public segment in its
// own namespace; put private segments in routers. Multiple private segments
// compose double NAT, while multiple peers on one segment model shared NAT.
type Segment struct {
	NS     *Namespace
	Bridge string
}

func (n *Namespace) Segment(bridge, cidr string) (*Segment, error) {
	if _, err := n.Run("ip", "link", "add", bridge, "type", "bridge"); err != nil {
		return nil, err
	}
	if _, err := n.Run("ip", "link", "set", bridge, "up"); err != nil {
		return nil, err
	}
	if cidr != "" {
		if _, err := n.Run("ip", "addr", "add", cidr, "dev", bridge); err != nil {
			return nil, err
		}
	}
	return &Segment{NS: n, Bridge: bridge}, nil
}
func (s *Segment) Attach(n *Namespace, iface, cidr, gateway string) error {
	l := n.lab
	l.serial++
	a, b := fmt.Sprintf("v%da", l.serial), fmt.Sprintf("v%db", l.serial)
	// Create inside the segment's namespace so partial failures are cleaned up
	// when it dies. Names need only be unique inside this lab.
	if _, err := s.NS.Run("ip", "link", "add", a, "type", "veth", "peer", "name", b); err != nil {
		return err
	}
	if _, err := s.NS.Run("ip", "link", "set", a, "master", s.Bridge); err != nil {
		return err
	}
	if _, err := s.NS.Run("ip", "link", "set", a, "up"); err != nil {
		return err
	}
	if _, err := s.NS.Run("ip", "link", "set", b, "netns", strconv.Itoa(n.pid)); err != nil {
		return err
	}
	for _, args := range [][]string{{"link", "set", b, "name", iface}, {"addr", "add", cidr, "dev", iface}, {"link", "set", iface, "up"}} {
		if _, err := n.Run("ip", args...); err != nil {
			return err
		}
	}
	if gateway != "" {
		_, err := n.Run("ip", "route", "replace", "default", "via", gateway)
		return err
	}
	return nil
}

// Link and address events, plus arbitrary route scripting through Run, can be
// applied while helper processes are transferring data. No sleeps are imposed.
func (n *Namespace) Link(iface string, up bool) error {
	state := "down"
	if up {
		state = "up"
	}
	_, err := n.Run("ip", "link", "set", iface, state)
	return err
}
func (n *Namespace) Address(iface, oldCIDR, newCIDR string) error {
	if oldCIDR != "" {
		if _, err := n.Run("ip", "addr", "del", oldCIDR, "dev", iface); err != nil {
			return err
		}
	}
	if newCIDR != "" {
		_, err := n.Run("ip", "addr", "add", newCIDR, "dev", iface)
		return err
	}
	return nil
}

// Start launches a process with pipes for a caller-defined control protocol.
// Cleanup kills and reaps it before releasing any network namespaces. Stderr
// goes to the test's stderr; stdout is reserved for the machine control channel.
func (n *Namespace) Start(program string, args ...string) (io.WriteCloser, io.ReadCloser, error) {
	cmd := n.Command(program, args...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, nil, err
	}
	n.lab.cleanups = append(n.lab.cleanups, func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait(); out.Close() })
	return in, out, nil
}

// Outcome is NDJSON-friendly evidence, not a derived NAT classification.
// Durations are nanoseconds; zero means not measured, not instantaneous success.
type Outcome struct {
	Scenario        string        `json:"scenario"`
	From            string        `json:"from"`
	To              string        `json:"to"`
	Outcome         string        `json:"outcome"`
	Elapsed         time.Duration `json:"elapsed_ns"`
	TimeToFirstByte time.Duration `json:"time_to_first_byte_ns,omitempty"`
	TimeToDirect    time.Duration `json:"time_to_direct_ns,omitempty"`
}
