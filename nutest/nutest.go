// Package nutest starts in-process multi-node Nu networks for tests: real nodes (store,
// swarm, optional API/gateway) under temporary $NU_PATHs, listening on loopback QUIC with
// mDNS off, peered explicitly through link hints. Nodes can be stopped and restarted with
// their store intact, killed without a clean shutdown, partitioned and healed through the
// host's connection gating, or made to serve corrupt bytes. Everything random derives from
// the network's seed.
//
// A typical test:
//
//	n := nutest.New(t, 3)
//	c := n.Nodes[0].AddBytes(n.RandBytes(1 << 20))
//	n.Nodes[2].Get(n.Link(c, n.Nodes[0]), dir)
package nutest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/swarm"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// Timeout bounds every wait in the harness.
const Timeout = 10 * time.Second

// Options configure a Network.
type Options struct {
	// Seed drives node keys and RandBytes (default 1).
	Seed int64
	// API also serves each node's local API and gateway (through node/daemon).
	API bool
	// Fetch tunes every node's fetcher (smaller chunks make mid-transfer faults easy).
	Fetch fetch.Config
}

// Network is a set of nodes in one process. Nodes are stopped at test cleanup.
type Network struct {
	T     testing.TB
	Nodes []*Node
	Opts  Options
	rng   *rand.Rand
	mu    sync.Mutex
}

// New starts n nodes.
func New(t testing.TB, n int, opts ...Options) *Network {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	nw := &Network{T: t, Opts: o, rng: rand.New(rand.NewSource(o.Seed))}
	for i := range n {
		nw.Nodes = append(nw.Nodes, nw.newNode(i))
	}
	t.Cleanup(func() {
		if t.Failed() {
			nw.Dump()
		}
		for _, nd := range nw.Nodes {
			nd.Stop()
		}
	})
	for _, nd := range nw.Nodes {
		nd.Start()
	}
	return nw
}

// RandBytes returns deterministic pseudo-random content.
func (nw *Network) RandBytes(n int) []byte {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	b := make([]byte, n)
	nw.rng.Read(b)
	return b
}

// Link is a share link for c naming the given nodes as providers (their loopback
// addresses as hints).
func (nw *Network) Link(c cid.CID, providers ...*Node) string {
	l := &link.Link{CID: c}
	for _, p := range providers {
		l.Providers = append(l.Providers, p.Provider())
	}
	return l.String()
}

// Peer makes a dial b and waits for the connection.
func (nw *Network) Peer(a, b *Node) {
	nw.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	if err := a.Swarm.Connect(ctx, b.ID, b.Addrs()); err != nil {
		nw.T.Fatalf("%s cannot connect to %s: %v", a.Name, b.Name, err)
	}
}

// Partition denies connections between a and b (existing ones are closed) until Heal.
func (nw *Network) Partition(a, b *Node) {
	a.deny(b.ID, true)
	b.deny(a.ID, true)
}

// Heal lifts a Partition.
func (nw *Network) Heal(a, b *Node) {
	a.deny(b.ID, false)
	b.deny(a.ID, false)
}

// Dump prints every node's status and operations (called automatically when the test
// fails).
func (nw *Network) Dump() {
	for _, nd := range nw.Nodes {
		nw.T.Logf("node %s (%s): %s", nd.Name, nd.ID.Base36(), nd.describe())
	}
}

// Eventually polls cond until it holds or Timeout expires.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Node is one node of a Network. Local, Swarm and Daemon are nil while it is stopped.
type Node struct {
	nw   *Network
	Name string
	// Dir is the node's $NU_PATH; it survives Stop/Kill so a restart keeps the store.
	Dir    string
	Repo   *repo.Repo
	Key    *wire.NodeKey
	ID     wire.PeerID
	Port   int
	Local  *node.Local
	Swarm  *swarm.Swarm
	Daemon *daemon.Daemon

	mu      sync.Mutex
	denied  map[wire.PeerID]bool
	corrupt atomic.Bool
	// CorruptServed counts /nu/bytes/1 responses served while corrupting.
	CorruptServed atomic.Int64
	lock          *repo.Lock
}

func (nw *Network) newNode(i int) *Node {
	t := nw.T
	t.Helper()
	nd := &Node{nw: nw, Name: fmt.Sprintf("n%d", i), Dir: filepath.Join(t.TempDir(), fmt.Sprintf("nu%d", i)), denied: map[wire.PeerID]bool{}}
	r, _, err := repo.Open(nd.Dir)
	if err != nil {
		t.Fatal(err)
	}
	nd.Repo = r
	var seed [ed25519.SeedSize]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(nw.Opts.Seed))
	binary.LittleEndian.PutUint64(seed[8:], uint64(i)+1)
	priv := ed25519.NewKeyFromSeed(seed[:])
	nd.Key, err = wire.ParseNodeKey(wire.AppendBytes(wire.AppendUint(nil, 1, uint64(wire.Ed25519)), 2, priv), wire.MaxKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := nd.Key.Store(r.Path(repo.KeyFile)); err != nil {
		t.Fatal(err)
	}
	nd.ID = nd.Key.Public().PeerID()
	// A fixed loopback port keeps the node's address stable across restarts.
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	nd.Port = pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return nd
}

func (nd *Node) listen() string { return fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", nd.Port) }

// Addrs are the node's listen addresses (stable across restarts).
func (nd *Node) Addrs() []wire.Multiaddr {
	a, err := wire.ParseMultiaddr(nd.listen(), 1024)
	if err != nil {
		panic(err)
	}
	return []wire.Multiaddr{a}
}

// Provider is this node as a link hint.
func (nd *Node) Provider() link.Provider { return link.Provider{Peer: nd.ID, Addrs: nd.Addrs()} }

// Running reports whether the node is started.
func (nd *Node) Running() bool { return nd.Local != nil }

// Start starts (or restarts) the node on its directory and port.
func (nd *Node) Start() {
	t := nd.nw.T
	t.Helper()
	if nd.Running() {
		t.Fatalf("%s is already running", nd.Name)
	}
	configure := func(c *swarm.Config) {
		c.Listen = []string{nd.listen()}
		c.MDNS = false
		c.Fetch = nd.nw.Opts.Fetch
		c.AllowPeer = nd.allow
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if nd.nw.Opts.API {
		d, err := daemon.Start(context.Background(), daemon.Options{Repo: nd.Repo, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0",
			Swarm: configure, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatalf("start %s: %v", nd.Name, err)
		}
		nd.Daemon, nd.Local, nd.Swarm = d, d.Node, d.Swarm
	} else {
		lock, err := nd.Repo.Lock()
		if err != nil {
			t.Fatalf("start %s: %v", nd.Name, err)
		}
		nd.lock = lock
		l, err := node.NewLocal(nd.Repo)
		if err != nil {
			t.Fatalf("start %s: %v", nd.Name, err)
		}
		cfg, err := swarm.RepoConfig(nd.Repo, l.Key, l.Store)
		if err != nil {
			t.Fatal(err)
		}
		configure(&cfg)
		sw, err := swarm.Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("start %s swarm: %v", nd.Name, err)
		}
		l.Fetcher, l.Net = sw, sw
		nd.Local, nd.Swarm = l, sw
	}
	// Serve /nu/bytes/1 through a stream wrapper so Corrupt can flip bytes.
	h := nd.Swarm.Host()
	srv := nd.Swarm.Server()
	h.RemoveHandler(proto.BytesID)
	if err := h.Handle(proto.BytesID, srv.HandlerLimit(), func(st *host.Stream) {
		if nd.corrupt.Load() {
			nd.CorruptServed.Add(1)
			srv.Serve(proto.BytesID, st.Conn().PeerID(), corruptStream{st})
			return
		}
		srv.Serve(proto.BytesID, st.Conn().PeerID(), st)
	}); err != nil {
		t.Fatal(err)
	}
}

// Stop shuts the node down cleanly (operations cancelled and waited for, swarm closed,
// store closed). The directory stays for Start.
func (nd *Node) Stop() {
	if !nd.Running() {
		return
	}
	if nd.Daemon != nil {
		_ = nd.Daemon.Close()
	} else {
		nd.Local.Ops.CancelAll()
		nd.Local.Wait()
		_ = nd.Swarm.Close()
		_ = nd.Local.Close()
		_ = nd.lock.Release()
	}
	nd.Daemon, nd.Local, nd.Swarm, nd.lock = nil, nil, nil, nil
}

// Kill stops the node without a clean shutdown: the host's sockets close first, so peers
// see connections die mid-request and running operations fail on a closed network, then
// the rest is torn down without waiting for them. The store is not flushed beyond what
// its own writes committed.
func (nd *Node) Kill() {
	if !nd.Running() {
		return
	}
	_ = nd.Swarm.Host().Close()
	_ = nd.Local.Store.Close()
	nd.Stop()
}

// Restart is Stop then Start.
func (nd *Node) Restart() {
	nd.Stop()
	nd.Start()
}

// Corrupt makes the node flip a byte in every /nu/bytes/1 response (or stop doing so).
func (nd *Node) Corrupt(on bool) { nd.corrupt.Store(on) }

func (nd *Node) allow(id wire.PeerID) bool {
	nd.mu.Lock()
	defer nd.mu.Unlock()
	return !nd.denied[id]
}

func (nd *Node) deny(id wire.PeerID, on bool) {
	nd.mu.Lock()
	nd.denied[id] = on
	nd.mu.Unlock()
	if nd.Running() {
		nd.Swarm.Host().RecheckConnections()
	}
}

// ConnectedTo reports whether the node has a live connection to id.
func (nd *Node) ConnectedTo(id wire.PeerID) bool {
	if !nd.Running() {
		return false
	}
	_, ok := nd.Swarm.Host().Connection(id)
	return ok
}

// Has reports whether c is held complete (nu-tree walk) in the node's store.
func (nd *Node) Has(c cid.CID) bool {
	st, err := nd.Local.Store.Walk(context.Background(), store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree"}, nil)
	return err == nil && st.Complete()
}

// AddBytes adds data as a file and returns its CID.
func (nd *Node) AddBytes(data []byte) cid.CID {
	nd.nw.T.Helper()
	p := filepath.Join(nd.nw.T.TempDir(), "file")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		nd.nw.T.Fatal(err)
	}
	return nd.Add(p, false)
}

// Add adds a path (recursively for a directory) and returns the root CID.
func (nd *Node) Add(path string, recursive bool) cid.CID {
	t := nd.nw.T
	t.Helper()
	var res apiv0.AddResult
	nd.run(func(ctx context.Context) (*apiv0.Operation, error) {
		return nd.Local.Add(ctx, apiv0.AddRequest{Path: path, Recursive: recursive, Pin: true})
	}, &res)
	c, err := cid.Parse(res.CID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Get runs a get of target (CID or link) to out and returns the result; it fails the test
// on error. TryGet returns the error instead.
func (nd *Node) Get(target, out string) apiv0.GetResult {
	nd.nw.T.Helper()
	res, err := nd.TryGet(target, out)
	if err != nil {
		nd.nw.T.Fatalf("%s get %s: %v", nd.Name, target, err)
	}
	return res
}

func (nd *Node) TryGet(target, out string) (apiv0.GetResult, error) {
	var res apiv0.GetResult
	err := nd.tryRun(func(ctx context.Context) (*apiv0.Operation, error) {
		return nd.Local.Get(ctx, apiv0.GetRequest{Target: target, Output: out})
	}, &res)
	return res, err
}

// Fetch brings c into the store through the node's fetcher with the given providers as
// hints; progress may be nil.
func (nd *Node) Fetch(c cid.CID, progress func(done, total int64), providers ...*Node) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	var hints []link.Provider
	for _, p := range providers {
		hints = append(hints, p.Provider())
	}
	return nd.Local.Fetcher.Fetch(ctx, c, hints, progress)
}

// Cat returns a file's bytes.
func (nd *Node) Cat(target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	rc, err := nd.Local.Cat(ctx, target)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(rc)
	return buf.Bytes(), err
}

func (nd *Node) run(start func(context.Context) (*apiv0.Operation, error), result any) {
	nd.nw.T.Helper()
	if err := nd.tryRun(start, result); err != nil {
		nd.nw.T.Fatalf("%s: %v", nd.Name, err)
	}
}

func (nd *Node) tryRun(start func(context.Context) (*apiv0.Operation, error), result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	o, err := start(ctx)
	if err != nil {
		return err
	}
	final, err := nd.Local.WaitOperation(ctx, o.ID, Timeout)
	if err != nil {
		return err
	}
	if final.State != apiv0.StateSucceeded {
		if final.Error != nil {
			return final.Error
		}
		return fmt.Errorf("operation %s %s", final.ID, final.State)
	}
	return json.Unmarshal(final.Result, result)
}

func (nd *Node) describe() string {
	if !nd.Running() {
		return "stopped"
	}
	st, err := nd.Local.Status(context.Background())
	if err != nil {
		return "status: " + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "addrs %v peers %v store %d objects %d pins", st.Network.Addrs, st.Network.Connected, st.Store.Objects, st.Store.Pins)
	for _, o := range nd.Local.Ops.List() {
		fmt.Fprintf(&b, "\n  op %s %s %s %s %d/%d %s", o.ID, o.Kind, o.Target, o.State, o.Progress.Done, o.Progress.Total, o.Stage)
		if o.Error != nil {
			fmt.Fprintf(&b, " error: %s", o.Error.Message)
		}
	}
	return b.String()
}

// corruptStream flips the last byte of every sizeable write, so response headers stay
// well-formed and the data fails verification.
type corruptStream struct{ *host.Stream }

func (c corruptStream) Write(b []byte) (int, error) {
	if len(b) > 512 {
		b = bytes.Clone(b)
		b[len(b)-1] ^= 0xff
	}
	return c.Stream.Write(b)
}
