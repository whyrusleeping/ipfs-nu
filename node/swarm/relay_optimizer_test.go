package swarm

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func pathStatus(s *Swarm, peer wire.PeerID) (row apiPath, ok bool) {
	for _, p := range s.ConnectivityStatus().Paths {
		if p.PeerID == peer.Base36() {
			return apiPath{p.Path, p.Relay, p.RelayMode, p.RelayChoice}, true
		}
	}
	return apiPath{}, false
}

type apiPath struct{ Path, Relay, Mode, Choice string }

func waitStatus(t *testing.T, ctx context.Context, what string, fn func() bool) {
	t.Helper()
	for !fn() {
		select {
		case <-ctx.Done():
			t.Fatal(what)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Two swarms through one admitted relay: the circuit dialer upgrades to the
// datagram extension, the target attaches the relay's offer, status reports it
// on both sides, and the inner connection keeps working.
func TestSwarmDatagramRelayUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	ka, kb := swarmKey(t), swarmKey(t)
	r := relayFor(t, ka.Public().PeerID(), kb.Public().PeerID())
	a, b := testSwarm(t, ka, relayAddr(t, r)), testSwarm(t, kb, relayAddr(t, r))
	hints := circuitHints(t, ctx, b, 1)
	if err := b.Host().Handle("/test/echo/1", 1, func(s *host.Stream) { io.Copy(s, s) }); err != nil {
		t.Fatal(err)
	}
	if err := a.Connect(ctx, b.ID(), hints); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, ctx, "datagram mode not reached on both sides", func() bool {
		pa, _ := pathStatus(a, b.ID())
		pb, _ := pathStatus(b, a.ID())
		return pa.Mode == "datagram" && pb.Mode == "datagram"
	})
	stream, err := a.Sessions().OpenStream(ctx, b.ID(), "/test/echo/1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	payload := make([]byte, 64<<10)
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() { _, _ = stream.Write(payload) }()
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, echoed); err != nil {
		t.Fatal(err)
	}
	sa, sb := a.connectivity.socket.Stats(), b.connectivity.socket.Stats()
	if sa.DatagramSent == 0 || sb.DatagramSent == 0 || sa.DatagramReceived == 0 || sb.DatagramReceived == 0 {
		t.Fatal("datagram leg idle", sa, sb)
	}
	if r.Circuit.DatagramLegs() != 1 {
		t.Fatal("relay legs", r.Circuit.DatagramLegs())
	}
	pa, _ := pathStatus(a, b.ID())
	if pa.Path != "relay" || pa.Relay != r.Host.ID().Base36() {
		t.Fatal(pa)
	}
	t.Logf("64 KiB echoed over the datagram relay leg: A sent/recv=%d/%d B=%d/%d; status %+v", sa.DatagramSent, sa.DatagramReceived, sb.DatagramSent, sb.DatagramReceived, pa)
}

// Pair-aware choice: A's home relay is far from B. Both reach a second relay;
// after the inner connection A fetches B's table and re-homes the leg onto the
// relay with the smaller combined RTT. A peer relay (a public swarm with an
// allow-list) is that better relay here, so the same test covers peer relays.
func TestSwarmPairAwareRelayChoiceWithPeerRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relayOptimizeDelay = 200 * time.Millisecond
	t.Cleanup(func() { relayOptimizeDelay = time.Second })
	ka, kb, ks := swarmKey(t), swarmKey(t), swarmKey(t)
	// Fake distance: the far relay's measured RTT is inflated by its ping
	// handler latency on both peers (loopback has no real distance).
	far := relayWithDelay(t, 30*time.Millisecond, ka.Public().PeerID(), kb.Public().PeerID())
	seed, err := Start(context.Background(), Config{Key: ks, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, Logger: discard, PeerRelayAllow: []wire.PeerID{ka.Public().PeerID(), kb.Public().PeerID()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.Close() })
	if seed.peerRelay == nil {
		t.Fatal("peer relay not started")
	}
	seedAddr, err := wire.ParseMultiaddr(seed.Host().Addrs()[0].String()+"/p2p/"+seed.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	a := testSwarm(t, ka, relayAddr(t, far), seedAddr)
	b := testSwarm(t, kb, relayAddr(t, far), seedAddr)
	hints := circuitHints(t, ctx, b, 2)
	var farHint wire.Multiaddr
	for _, h := range hints {
		if _, rid, _, err := circuit.ParseAddress(h); err == nil && rid == far.Host.ID() {
			farHint = h
		}
	}
	if farHint.Bytes() == nil {
		t.Fatal("B holds no reservation at the far relay", hints)
	}
	if err := a.Connect(ctx, b.ID(), []wire.Multiaddr{farHint}); err != nil {
		t.Fatal(err)
	}
	pa, _ := pathStatus(a, b.ID())
	if pa.Relay != far.Host.ID().Base36() {
		t.Fatal("initial leg not through the far relay", pa)
	}
	before, _ := a.Host().Connection(b.ID())
	waitStatus(t, ctx, "pair did not move to the lower-RTT peer relay", func() bool {
		pa, _ = pathStatus(a, b.ID())
		pb, _ := pathStatus(b, a.ID())
		return pa.Relay == seed.ID().Base36() && pb.Relay == seed.ID().Base36() && pa.Path == "relay"
	})
	after, ok := a.Host().Connection(b.ID())
	if !ok || after != before {
		t.Fatal("inner connection replaced during relay switch")
	}
	if seed.peerRelay.DatagramLegs() == 0 {
		waitStatus(t, ctx, "datagram mode on the peer relay", func() bool { return seed.peerRelay.DatagramLegs() == 1 })
	}
	rtt, err := a.Ping(ctx, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("re-homed from far relay to peer relay (seed) without replacing inner QUIC: %+v; inner RTT now %v", pa, rtt)
}
