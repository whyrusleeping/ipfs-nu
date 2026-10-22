package disco

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestPartialHardCapabilityKeepsV1(t *testing.T) {
	var ca, cb atomic.Value
	ca.Store([]Candidate(nil))
	cb.Store([]Candidate(nil))
	a, b, c := innerPair(t, func(wire.PeerID) []Candidate { return ca.Load().([]Candidate) }, func(wire.PeerID) []Candidate { return cb.Load().([]Candidate) })
	ca.Store([]Candidate{candidate(a.udp)})
	cb.Store([]Candidate{candidate(b.udp)})
	// Simulate preflight's final response being lost/reset at one endpoint.
	a.disco.mu.Lock()
	a.disco.capabilities = map[*host.Conn]time.Time{c: time.Now().Add(time.Second)}
	a.disco.mu.Unlock()
	check(t, a.disco.Open(context.Background(), c))
	eventually(t, func() bool {
		left, _ := a.disco.Status(b.host.ID())
		right, _ := b.disco.Status(a.host.ID())
		return left.Selected == b.udp && right.Selected == a.udp && left.HardSent == 0 && right.HardSent == 0
	})
}

func TestHardConsentBoundsAndBackoff(t *testing.T) {
	d, s, dest := securityFixture(t)
	now := time.Now()
	from := dest.LocalAddr().(*net.UDPAddr).AddrPort()
	base := Candidate{Kind: Reflexive, Addr: from, Expires: now.Add(time.Minute)}
	d.cfg.Hard = &HardConfig{Authorize: func(_ wire.PeerID, ip netip.Addr) bool { return ip == from.Addr() }}
	d.hardPeers = make(map[wire.PeerID]hardPeerState)
	d.ids = make(map[[16]byte]*session)
	s.hardPending = make(map[[16]byte]hardChallenge)
	s.asserted = map[netip.AddrPort]Candidate{from: base}
	s.remoteGen = 1
	s.remoteHard = &hardOffer{profile: netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingEndpointIndependent, Allocation: netcheck.AllocPreserving}, base: base, predicted: from.Port(), generation: 1}
	s.localHard = &hardOffer{profile: netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingAddressPortDependent, Allocation: netcheck.AllocRandom}, base: base, predicted: from.Port(), generation: 1}
	id := [16]byte{42}
	if d.hardControl(s, []byte{1, 2}, now) {
		t.Fatal("unnegotiated extension accepted")
	}
	s.hardCap = true
	if !d.reserveHard(s, id, now) {
		t.Fatal("first attempt rejected")
	}
	// A replacement session cannot reset the peer cooldown, and an old attempt
	// ID cannot be reused even after the cooldown within the same key session.
	if d.reserveHard(&session{peer: s.peer}, [16]byte{43}, now.Add(time.Second)) || d.reserveHard(s, id, now.Add(time.Hour)) {
		t.Fatal("cooldown/replay bypass")
	}
	s.hard = &hardAttempt{id: id, phase: 5, deadline: now.Add(time.Minute)}
	s.hardID = id
	d.hardActive = s
	other := netip.MustParseAddrPort("127.0.0.2:4000")
	if d.hardSend(s, probe{kind: 1}, other, nil, now) || d.hardAllowed(s, netip.AddrPortFrom(from.Addr(), 80), now) {
		t.Fatal("NAT IP/port restriction bypassed")
	}
	valid := encodeProbe(probe{kind: 1, session: id, seq: 1, challenge: [16]byte{9}}, s.rx)
	forged := append([]byte(nil), valid...)
	forged[70] ^= 1
	d.handleHardProbe(s, forged, from, nil, now)
	d.handleHardProbe(s, valid, other, nil, now)
	if s.stats.HardSent != 0 || len(s.proofs) != 0 {
		t.Fatal("forged or unauthorized probe elicited traffic")
	}
	d.handleHardProbe(s, valid, from, nil, now)
	before := s.stats.HardSent
	if before != 2 || len(s.proofs) != 0 {
		t.Fatal("ping must only answer/challenge, never admit a path")
	}
	d.handleHardProbe(s, valid, from, nil, now)
	if s.stats.HardSent != before {
		t.Fatal("replay elicited traffic")
	}
	// A valid MAC cannot move a challenge to a different local socket.
	var nonce [16]byte
	nonce[0] = 7
	s.hardPending[nonce] = hardChallenge{from, dest, now}
	d.handleHardProbe(s, encodeProbe(probe{kind: 2, session: id, seq: 2, challenge: nonce, observed: from}, s.rx), from, nil, now)
	if len(s.proofs) != 0 {
		t.Fatal("local socket mismatch installed a path")
	}
	// Sending is bounded even when a peer answers everything. Pongs consume the
	// same packet budget, including failed sends; token caps apply independently.
	for i := 0; i < hardPacketLimit+20; i++ {
		d.hardSend(s, probe{kind: 2, observed: from}, from, nil, now.Add(time.Duration(i)*time.Millisecond))
	}
	if s.stats.HardSent > hardPacketLimit || s.hard == nil && s.stats.HardBackoffs != 1 {
		t.Fatal("packet cap/backoff", s.stats)
	}
	if s.hard != nil { // advance past token starvation to exercise exact cap
		s.hard.sent = hardPacketLimit
		d.hardSend(s, probe{kind: 2, observed: from}, from, nil, now.Add(5*time.Second))
	}
	if s.hard != nil || s.stats.HardBackoffs != 1 || !s.hardNext.After(now) {
		t.Fatal("budget exhaustion did not cancel/back off")
	}
	// With the service-wide socket cap already occupied by retained winners,
	// no new socket is opened. All temporary resources remain accounted for.
	s.hard = &hardAttempt{id: [16]byte{44}}
	d.hardSockets = hardSocketLimit
	d.startHard(s, now)
	if s.hard != nil || s.stats.HardSocketPeak != 0 || d.hardSockets != hardSocketLimit {
		t.Fatal("socket cap bypassed")
	}
	for _, p := range predictionPorts(1024, 64) {
		if p < 1024 {
			t.Fatal("prediction wrapped into privileged ports")
		}
	}
	ports := randomPorts()
	seen := map[uint16]bool{}
	for _, p := range ports {
		if p < 1024 || seen[p] {
			t.Fatal("random port domain/uniqueness")
		}
		seen[p] = true
	}
	if len(ports) != hardSearchPorts || hardPair(netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingAddressPortDependent, Allocation: netcheck.AllocRandom}, netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingAddressPortDependent, Allocation: netcheck.AllocRandom}) {
		t.Fatal("search bound/both-random policy")
	}
}
