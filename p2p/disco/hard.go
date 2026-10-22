package disco

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// HardCapability is only a backwards-compatible feature handshake on the exact
// inner TLS connection. Offers and attempt consent then travel on /nu/path/1.
const HardCapability = "/nu/path/hard/1"

const (
	hardSocketLimit = 256  // per attempt AND service, including retained winners
	hardPacketLimit = 4096 // per direction/attempt, including pongs and failed sends
	hardSearchPorts = 1024
	hardDuration    = 8 * time.Second
)

// HardConfig is opt-in at the library boundary. Authorize independently permits
// one peer-reported NAT IP, not a subnet or a candidate supplied by incoming UDP.
// It must exclude sensitive destinations for the deployment. Profile must return
// quickly and must not call Service methods. ListenUDP is a test seam; nil uses
// a wildcard IPv4 socket. The service owns and closes every opened socket.
type HardConfig struct {
	Profile   func() (netcheck.Report, bool)
	Authorize func(wire.PeerID, netip.Addr) bool
	ListenUDP func() (*net.UDPConn, error)
}

type hardOffer struct {
	profile    netcheck.Profile
	base       Candidate
	predicted  uint16
	generation uint64
}

type hardAttempt struct {
	id, hash       [16]byte
	phase          byte // 3=request, 4=accepted, 5=committed
	deadline, next time.Time
	ports          []uint16
	index          int
	fanout         bool
	sockets        []*net.UDPConn
	sent           uint64
	errors         int
}

// Cooldown survives control-stream replacement, so reopening a session cannot
// refill its attempt budget. One bounded slot per recently attempted peer.
type hardPeerState struct {
	until    time.Time
	failures int
	id       [16]byte
}

func (d *Service) reserveHard(s *session, id [16]byte, now time.Time) bool {
	if s.hardSeen == nil {
		s.hardSeen = make(map[[16]byte]struct{})
	}
	if _, seen := s.hardSeen[id]; seen || len(s.hardSeen) >= 64 {
		return false
	}
	state, exists := d.hardPeers[s.peer]
	if now.Before(state.until) || id == state.id {
		return false
	}
	if !exists && len(d.hardPeers) >= 128 {
		for peer, p := range d.hardPeers {
			if now.After(p.until.Add(10 * time.Minute)) {
				delete(d.hardPeers, peer)
			}
		}
		if len(d.hardPeers) >= 128 {
			return false
		}
	}
	state.id, state.until = id, now.Add(retries[min(state.failures, len(retries)-1)])
	state.failures++
	d.hardPeers[s.peer] = state
	s.hardSeen[id] = struct{}{}
	return true
}

type hardChallenge struct {
	addr  netip.AddrPort
	local *net.UDPConn // nil is the primary shared socket
	sent  time.Time
}

func (d *Service) initHard() error {
	if d.cfg.Hard == nil {
		return nil
	}
	if d.cfg.Hard.Profile == nil || d.cfg.Hard.Authorize == nil {
		return errors.New("disco: hard NAT requires a profile and destination policy")
	}
	d.capabilities = make(map[*host.Conn]time.Time)
	d.hardPeers = make(map[wire.PeerID]hardPeerState)
	return d.cfg.Host.Handle(HardCapability, 16, func(st *host.Stream) {
		defer st.Close()
		_ = st.SetDeadline(time.Now().Add(time.Second))
		if !d.inner(st.Conn()) {
			st.Reset()
			return
		}
		d.mu.Lock()
		ok := d.rememberCapability(st.Conn())
		d.mu.Unlock()
		if ok {
			_, _ = st.Write([]byte{1})
		}
	})
}
func (d *Service) inner(c *host.Conn) bool {
	v, ok := d.cfg.Socket.PeerAddr(c.PeerID())
	a, udp := c.RemoteAddr().(*net.UDPAddr)
	return ok && udp && netip.AddrPortFrom(a.AddrPort().Addr().Unmap(), a.AddrPort().Port()) == v
}
func (d *Service) rememberCapability(c *host.Conn) bool {
	now := time.Now()
	for conn, until := range d.capabilities {
		if !now.Before(until) || conn.Context().Err() != nil {
			delete(d.capabilities, conn)
		}
	}
	if d.closed || len(d.capabilities) >= 128 {
		return false
	}
	d.capabilities[c] = now.Add(10 * time.Second)
	return true
}
func (d *Service) negotiateHard(ctx context.Context, c *host.Conn) {
	if d.cfg.Hard == nil || !d.inner(c) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	st, err := c.OpenStream(ctx)
	if err != nil {
		return
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(time.Second))
	if wire.Select(st, HardCapability) != nil {
		return
	}
	var b [1]byte
	if _, err := io.ReadFull(st, b[:]); err != nil || b[0] != 1 {
		return
	}
	d.mu.Lock()
	d.rememberCapability(c)
	d.mu.Unlock()
}
func offerBytes(o *hardOffer) []byte {
	b := make([]byte, 49)
	b[0], b[1] = 1, 2
	binary.BigEndian.PutUint64(b[2:10], o.generation)
	p, _ := o.profile.MarshalBinary()
	copy(b[10:28], p)
	putEndpoint(b[28:47], o.base.Addr)
	binary.BigEndian.PutUint16(b[47:49], o.predicted)
	return b
}
func (d *Service) refreshHardOffer(s *session, list []Candidate, now time.Time) {
	d.mu.Lock()
	enabled := s.hardCap && s.ready
	d.mu.Unlock()
	if !enabled {
		return
	}
	r, ok := d.cfg.Hard.Profile()
	if !ok || r.Profile.UDP != netcheck.UDPOK || r.Profile.VaryingIP || r.Profile.Validate() != nil {
		return
	}
	var o *hardOffer
	for _, c := range list {
		if c.Kind != Reflexive || !validEndpoint(c.Addr) || !c.Addr.Addr().Is4() || c.Addr.Port() < 1024 || !c.Expires.After(now) {
			continue
		}
		o = &hardOffer{profile: r.Profile, base: c, predicted: c.Addr.Port(), generation: s.localGen}
		break
	}
	if o == nil {
		return
	}
	if r.Profile.Allocation == netcheck.AllocSequential && len(r.IPv4.Samples) > 0 {
		last := r.IPv4.Samples[len(r.IPv4.Samples)-1].Mapped
		p := int(last.Port()) + int(r.Profile.Delta)
		if last.Addr() == o.base.Addr.Addr() && p >= 1024 && p <= 65535 {
			o.predicted = uint16(p)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	s.localHard = o
	d.queueHard(s, offerBytes(o))
}
func (d *Service) queueHard(s *session, b []byte) bool {
	select {
	case s.controls <- b:
		return true
	default:
		return false
	}
}
func (d *Service) planHash(s *session) [16]byte {
	a, b := offerBytes(s.localHard), offerBytes(s.remoteHard)
	if d.cfg.Host.ID() > s.peer {
		a, b = b, a
	}
	h := sha256.Sum256(append(a, b...))
	return [16]byte(h[:16])
}
func hardFrame(kind byte, a *hardAttempt) []byte {
	b := make([]byte, 34)
	b[0], b[1] = 1, kind
	copy(b[2:18], a.id[:])
	copy(b[18:34], a.hash[:])
	return b
}
func eim(p netcheck.Profile) bool {
	return p.Mapping == netcheck.MappingNone || p.Mapping == netcheck.MappingEndpointIndependent
}
func predictable(p netcheck.Profile) bool {
	return p.Allocation == netcheck.AllocSequential || p.Allocation == netcheck.AllocPreserving
}
func hardPair(a, b netcheck.Profile) bool {
	ready := func(p netcheck.Profile) bool {
		return p.Mapping != netcheck.MappingUnknown && (eim(p) || predictable(p) || p.Allocation == netcheck.AllocRandom)
	}
	return a.UDP == netcheck.UDPOK && b.UDP == netcheck.UDPOK && ready(a) && ready(b) && !a.VaryingIP && !b.VaryingIP && !(eim(a) && eim(b)) && (eim(a) || eim(b) || predictable(a) || predictable(b))
}
func (d *Service) offersValid(s *session, now time.Time) bool {
	if !s.hardCap || s.localHard == nil || s.remoteHard == nil || !s.localHard.base.Expires.After(now) {
		return false
	}
	o := s.remoteHard
	c, ok := s.asserted[o.base.Addr]
	return ok && c.Kind == Reflexive && now.Before(c.Expires) && o.generation == s.remoteGen && d.cfg.Hard.Authorize(s.peer, c.Addr.Addr()) && hardPair(s.localHard.profile, o.profile)
}
func (d *Service) hardControl(s *session, b []byte, now time.Time) bool {
	if !s.hardCap || d.cfg.Hard == nil || len(b) < 2 || b[0] != 1 {
		return false
	}
	if b[1] == 2 {
		if len(b) != 49 {
			return false
		}
		p, err := netcheck.DecodeProfile(b[10:28])
		if err != nil {
			return false
		}
		base, err := getEndpoint(b[28:47])
		if err != nil || !base.Addr().Is4() || base.Port() < 1024 {
			return false
		}
		gen := binary.BigEndian.Uint64(b[2:10])
		predicted := binary.BigEndian.Uint16(b[47:49])
		c, ok := s.asserted[base]
		if gen != s.remoteGen || !ok || c.Kind != Reflexive || !c.Expires.After(now) || predicted < 1024 {
			return false
		}
		s.remoteHard = &hardOffer{p, c, predicted, gen}
		return true
	}
	if len(b) != 34 || b[1] < 3 || b[1] > 6 {
		return false
	}
	var id, hash [16]byte
	copy(id[:], b[2:18])
	copy(hash[:], b[18:34])
	if b[1] == 6 {
		if s.hard != nil && s.hard.id == id {
			d.failHard(s, "peer cancelled", now)
		}
		return true
	}
	if b[1] == 3 {
		if d.cfg.Host.ID() < s.peer {
			return false
		} // one proposer per session
		if !d.offersValid(s, now) || hash != d.planHash(s) || s.hard != nil || s.selected.IsValid() || now.Before(s.hardNext) || d.hardActive != nil || id == ([16]byte{}) || d.ids[id] != nil || !d.reserveHard(s, id, now) {
			return true
		}
		s.hard = &hardAttempt{id: id, hash: hash, phase: 4, deadline: now.Add(10 * time.Second)}
		d.hardActive = s
		s.stats.HardAttempts++
		return d.queueHard(s, hardFrame(4, s.hard))
	}
	a := s.hard
	if a == nil || a.id != id || a.hash != hash {
		return true
	} // stale acknowledgement
	if !d.offersValid(s, now) || hash != d.planHash(s) {
		d.failHard(s, "offer changed", now)
		return true
	}
	if b[1] == 4 && a.phase == 3 && d.cfg.Host.ID() < s.peer {
		if !d.queueHard(s, hardFrame(5, a)) {
			return false
		}
		a.phase = 4 // the writer starts UDP only after actually writing commit
	} else if b[1] == 5 && a.phase == 4 && d.cfg.Host.ID() > s.peer {
		d.startHard(s, now)
	}
	return true
}
func predictionPorts(port uint16, jitter uint16) []uint16 {
	width := max(64, min(256, int(jitter)*4))
	out := make([]uint16, 0, 2*width+1)
	for delta := 0; delta <= width; delta++ {
		for _, p := range []int{int(port) + delta, int(port) - delta} {
			if p >= 1024 && p <= 65535 && (delta != 0 || len(out) == 0) {
				out = append(out, uint16(p))
			}
		}
	}
	return out
}
func randomPorts() []uint16 {
	seen := make(map[uint16]bool, hardSearchPorts)
	out := make([]uint16, 0, hardSearchPorts)
	for len(out) < hardSearchPorts {
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			break
		}
		p := binary.BigEndian.Uint16(b[:])
		if p < 1024 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
func (d *Service) startHard(s *session, now time.Time) {
	a := s.hard
	a.phase, a.deadline, a.next = 5, now.Add(hardDuration), now
	a.fanout = !eim(s.localHard.profile) && !predictable(s.localHard.profile)
	if a.fanout {
		a.ports = []uint16{s.remoteHard.predicted}
	} else if eim(s.remoteHard.profile) {
		a.ports = []uint16{s.remoteHard.base.Addr.Port()}
	} else if predictable(s.remoteHard.profile) {
		a.ports = predictionPorts(s.remoteHard.predicted, s.remoteHard.profile.Jitter)
	} else {
		a.ports = randomPorts()
	}
	s.hardID = a.id
	s.hardReplay = replayWindow{}
	d.ids[a.id] = s
	s.stats.HardResult = "probing"
	if a.fanout {
		for range hardSocketLimit - d.hardSockets {
			var udp *net.UDPConn
			var err error
			if d.cfg.Hard.ListenUDP != nil {
				udp, err = d.cfg.Hard.ListenUDP()
			} else {
				udp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
			}
			if err != nil {
				break
			}
			a.sockets = append(a.sockets, udp)
			d.hardSockets++
			s.stats.HardSockets++
			s.stats.HardSocketPeak = max(s.stats.HardSocketPeak, s.stats.HardSockets)
			d.wg.Add(1)
			go d.readHardSocket(s, udp)
		}
		if len(a.sockets) == 0 {
			d.failHard(s, "socket budget exhausted", now)
		}
	}
}
func (d *Service) hardTick(s *session, now time.Time) {
	for n, p := range s.hardPending {
		if now.Sub(p.sent) >= d.cfg.ProbeTimeout {
			delete(s.hardPending, n)
			if proof, ok := s.proofs[p.addr]; ok && !proof.last.After(p.sent) {
				d.revoke(s)
				d.restartSchedule(s, now)
				return
			}
		}
	}
	a := s.hard
	if a != nil {
		if !now.Before(a.deadline) || !d.offersValid(s, now) {
			d.failHard(s, "timeout or changed offers (loss/IPS/NAT indistinguishable)", now)
			return
		}
		if a.phase != 5 || now.Before(a.next) {
			return
		}
		a.next = now.Add(10 * time.Millisecond)
		for range 4 {
			var local *net.UDPConn
			var port uint16
			if a.fanout {
				if a.index >= len(a.sockets) {
					return
				}
				local, port = a.sockets[a.index], a.ports[0]
			} else {
				if a.index >= len(a.ports) {
					if len(a.ports) != 1 {
						return
					}
					a.index = 0
					a.next = now.Add(50 * time.Millisecond)
				}
				port = a.ports[a.index]
			}
			if !d.hardPing(s, netip.AddrPortFrom(s.remoteHard.base.Addr.Addr(), port), local, now) {
				return
			}
			a.index++
		}
		return
	}
	if s.selected.IsValid() || d.hardActive != nil || now.Sub(s.start) < 3*time.Second || now.Before(s.hardNext) || !d.offersValid(s, now) || d.cfg.Host.ID() > s.peer {
		return
	}
	a = &hardAttempt{phase: 3, hash: d.planHash(s), deadline: now.Add(10 * time.Second)}
	if _, err := rand.Read(a.id[:]); err != nil {
		return
	}
	if a.id == ([16]byte{}) || d.ids[a.id] != nil || !d.reserveHard(s, a.id, now) {
		return
	}
	s.hard, d.hardActive = a, s
	s.stats.HardAttempts++
	if !d.queueHard(s, hardFrame(3, a)) {
		d.failHard(s, "control queue full", now)
	}
}
func (d *Service) hardAllowed(s *session, from netip.AddrPort, now time.Time) bool {
	if d.hardAuthorized(s, from, now) {
		return true
	}
	return s.hard != nil && s.hard.phase == 5 && now.Before(s.hard.deadline) && d.offersValid(s, now) && from.Addr() == s.remoteHard.base.Addr.Addr() && from.Port() >= 1024 && validEndpoint(from)
}
func (d *Service) hardAuthorized(s *session, from netip.AddrPort, now time.Time) bool {
	if !from.IsValid() || from != s.hardProven || d.cfg.Hard == nil {
		return false
	}
	anchor, ok := s.asserted[s.hardAnchor]
	return ok && anchor.Kind == Reflexive && now.Before(anchor.Expires) && d.cfg.Hard.Authorize(s.peer, from.Addr())
}
func (d *Service) hardSend(s *session, p probe, to netip.AddrPort, local *net.UDPConn, now time.Time) bool {
	if s.seq == math.MaxUint64 {
		s.dead = true
		s.stream.Reset()
		d.revoke(s)
		return false
	}
	if !d.hardAllowed(s, to, now) {
		return false
	}
	if s.hard != nil {
		if s.hard.sent >= hardPacketLimit || s.hard.errors >= 8 {
			d.failHard(s, "packet/error budget", now)
			return false
		}
		if !s.out.available(now, 512, 32) || !d.global.available(now, 512, 128) {
			return false
		}
		s.out.tokens--
		d.global.tokens--
		s.hard.sent++
	} else { // established-path keepalives retain the ordinary bounds
		if !s.out.take(now, 20, 32) || !d.global.take(now, 100, 128) {
			return false
		}
	}
	s.seq++
	p.seq, p.session = s.seq, s.hardID
	var err error
	if local == nil {
		err = d.cfg.Socket.WriteDisco(encodeProbe(p, s.tx), to)
	} else {
		var b []byte
		b, err = magicsock.EncodeDisco(encodeProbe(p, s.tx))
		if err == nil {
			_, err = local.WriteToUDPAddrPort(b, to)
		}
	}
	s.stats.HardSent++
	s.stats.Sent++
	d.observe(Event{Peer: s.peer, Kind: ProbeSent, Addr: to})
	if err != nil && s.hard != nil {
		s.hard.errors++
		return false
	}
	return err == nil
}
func (d *Service) hardPing(s *session, to netip.AddrPort, local *net.UDPConn, now time.Time) bool {
	if len(s.hardPending) >= hardSearchPorts+hardSocketLimit {
		return false
	}
	for _, p := range s.hardPending {
		if p.addr == to && p.local == local {
			return true
		}
	}
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return false
	}
	if !d.hardSend(s, probe{kind: 1, challenge: n}, to, local, now) {
		return false
	}
	s.hardPending[n] = hardChallenge{to, local, now}
	return true
}
func (d *Service) readHardSocket(s *session, udp *net.UDPConn) {
	defer d.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, from, err := udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		payload, err := magicsock.DecodeDisco(buf[:n])
		if err == nil {
			d.mu.Lock()
			if !d.closed && d.sessions[s.peer] == s {
				d.handleHardProbe(s, payload, from, udp, time.Now())
			}
			d.mu.Unlock()
		} else {
			d.cfg.Socket.InjectDirectSocket(s.peer, udp, from, buf[:n])
		}
	}
}
func (d *Service) handleHardProbe(s *session, b []byte, from netip.AddrPort, local *net.UDPConn, now time.Time) {
	if !d.hardAllowed(s, from, now) || (s.hard == nil && local != s.auxiliary) {
		s.stats.Rejected++
		return
	}
	p, err := decodeProbe(b, s.rx)
	if err != nil || p.session != s.hardID || !s.hardReplay.accept(p.seq) {
		s.stats.Rejected++
		return
	}
	s.stats.HardReceived++
	s.stats.Received++
	if p.kind == 1 {
		d.hardSend(s, probe{kind: 2, challenge: p.challenge, observed: from}, from, local, now)
		if s.hard != nil {
			d.hardPing(s, from, local, now)
		}
		return
	}
	pending, ok := s.hardPending[p.challenge]
	if !ok || pending.addr != from || pending.local != local || now.Sub(pending.sent) >= d.cfg.ProbeTimeout {
		s.stats.Rejected++
		return
	}
	delete(s.hardPending, p.challenge)
	s.proofs[from] = proof{now, max(time.Nanosecond, now.Sub(pending.sent))}
	if s.hard == nil {
		return
	}
	s.hardProven, s.hardAnchor = from, s.remoteHard.base.Addr
	c := s.asserted[s.hardAnchor]
	c.Addr = from
	s.remote[from] = c
	d.syncPaths(s)
	if local == nil {
		err = d.cfg.Socket.UseDirect(s.peer, from)
	} else {
		err = d.cfg.Socket.UseDirectSocket(s.peer, from, local)
	}
	if err != nil {
		d.failHard(s, "path installation failed", now)
		return
	}
	s.auxiliary = local
	s.selected, s.selectedAt = from, now
	if s.stats.TimeToDirect == 0 {
		s.stats.TimeToDirect = now.Sub(s.start)
	}
	for _, udp := range s.hard.sockets {
		if udp != local {
			udp.Close()
			d.hardSockets--
			s.stats.HardSockets--
		}
	}
	s.hard = nil
	if d.hardActive == s {
		d.hardActive = nil
	}
	clear(s.hardPending)
	s.stats.HardResult = "direct"
	state := d.hardPeers[s.peer]
	state.failures = 0
	s.hardFailures = 0
	d.hardPeers[s.peer] = state
	d.observe(Event{Peer: s.peer, Kind: PathDirect, Addr: from, Elapsed: now.Sub(s.start)})
}
func (d *Service) cancelHard(s *session) {
	if s.hard != nil {
		for _, udp := range s.hard.sockets {
			udp.Close()
			d.hardSockets--
			s.stats.HardSockets--
		}
		s.hard = nil
	}
	if s.auxiliary != nil {
		_ = d.cfg.Socket.SetDirectPaths(s.peer, nil)
		s.auxiliary.Close()
		s.auxiliary = nil
		d.hardSockets--
		s.stats.HardSockets--
	}
	if d.hardActive == s {
		d.hardActive = nil
	}
	if s.hardID != ([16]byte{}) {
		delete(d.ids, s.hardID)
	}
	s.hardID = [16]byte{}
	s.hardProven = netip.AddrPort{}
	s.hardAnchor = netip.AddrPort{}
	clear(s.hardPending)
}
func (d *Service) failHard(s *session, why string, now time.Time) {
	if s.hard != nil {
		d.queueHard(s, hardFrame(6, s.hard))
	}
	d.cancelHard(s)
	s.stats.HardResult = why
	s.stats.HardBackoffs++
	s.hardNext = now.Add(retries[min(s.hardFailures, len(retries)-1)])
	state := d.hardPeers[s.peer]
	if state.until.After(s.hardNext) {
		s.hardNext = state.until
	}
	state.until = s.hardNext
	d.hardPeers[s.peer] = state
	s.hardFailures++
}
