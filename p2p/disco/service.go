package disco

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Config deliberately requires owner policy. Callbacks must be concurrency-safe,
// quick, nonblocking and must not call Service methods. Candidates discloses
// private addresses only to the specified peer, never to global peer records.
type Config struct {
	Host         *host.Host
	Socket       *magicsock.Conn
	Candidates   func(wire.PeerID) []Candidate
	Authorize    func(wire.PeerID, Candidate) bool
	Heartbeat    time.Duration // zero: 25s; shorten for measured mapping lifetime
	ProbeTimeout time.Duration // zero: 3s
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) available(now time.Time, rate, burst float64) bool {
	if b.at.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = math.Min(burst, b.tokens+math.Max(0, now.Sub(b.at).Seconds())*rate)
	}
	b.at = now
	return b.tokens >= 1
}
func (b *bucket) take(now time.Time, rate, burst float64) bool {
	if !b.available(now, rate, burst) {
		return false
	}
	b.tokens--
	return true
}

type challenge struct {
	addr netip.AddrPort
	sent time.Time
}
type proof struct {
	last time.Time
	rtt  time.Duration
}
type session struct {
	stream                   *host.Stream
	peer                     wire.PeerID
	id, nonce                [16]byte
	ready                    bool
	done                     chan struct{}
	tx, rx                   []byte
	seq, remoteGen, localGen uint64
	replay                   replayWindow
	out, in                  bucket
	remote                   map[netip.AddrPort]Candidate
	pending                  map[[16]byte]challenge
	proofs                   map[netip.AddrPort]proof
	updates                  chan struct{}
	selected                 netip.AddrPort
	selectedAt               time.Time
	start, next, refresh     time.Time
	round, retry             int
	dead                     bool
	stats                    Status
}

// Status is a snapshot, including observed addresses for diagnostics only.
type Status struct {
	Selected                            netip.AddrPort
	Observed                            netip.AddrPort
	TimeToDirect                        time.Duration
	RTT                                 time.Duration
	Sent, Received, Rejected, Fallbacks uint64
	Candidates, Validated, Pending      int
}

// Service owns the sole disco queue consumer, bounded session state and workers.
// Close before closing Host/Socket; Close resets its control streams and joins
// workers, without closing the inner connections, relay legs or shared socket.
type Service struct {
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	sessions map[wire.PeerID]*session
	ids      map[[16]byte]*session
	global   bucket
	closed   bool
	wg       sync.WaitGroup
	once     sync.Once
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Host == nil || cfg.Socket == nil || cfg.Candidates == nil || cfg.Authorize == nil {
		return nil, errors.New("disco: host, socket, candidate provider and independent destination policy required")
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 25 * time.Second
	}
	if cfg.ProbeTimeout == 0 {
		cfg.ProbeTimeout = 3 * time.Second
	}
	if cfg.Heartbeat <= 0 || cfg.Heartbeat > 25*time.Second || cfg.ProbeTimeout <= 0 || cfg.ProbeTimeout > 3*time.Second {
		return nil, wire.ErrLimit
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &Service{cfg: cfg, ctx: ctx, cancel: cancel, sessions: make(map[wire.PeerID]*session), ids: make(map[[16]byte]*session)}
	if err := cfg.Host.Handle(Protocol, 128, d.handle); err != nil {
		cancel()
		return nil, err
	}
	d.wg.Add(2)
	go d.receive()
	go d.run()
	return d, nil
}
func (d *Service) Close() error {
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.cancel()
		for _, s := range d.sessions {
			s.stream.Reset()
			d.revoke(s)
		}
		d.mu.Unlock()
		d.cfg.Host.RemoveHandler(Protocol)
		d.wg.Wait()
	})
	return nil
}

// Open binds selection to this exact authenticated inner connection, not the
// Host's possibly different preferred connection to the same peer. The circuit
// owner designates one initiator to call Open (Swarm uses the lower inner TLS
// PeerID); the other endpoint's registered handler accepts.
func (d *Service) Open(ctx context.Context, c *host.Conn) error {
	s, err := c.OpenStream(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	_ = s.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, s.Reset)
	err = wire.Select(s, Protocol)
	if !stop() {
		s.Reset()
		return ctx.Err()
	}
	if err != nil {
		s.Reset()
		return err
	}
	_ = s.SetDeadline(time.Time{})
	p, err := d.admit(s)
	if err != nil {
		s.Reset()
		return err
	}
	go d.serve(p)
	return nil
}
func (d *Service) handle(s *host.Stream) {
	p, err := d.admit(s)
	if err != nil {
		s.Reset()
		return
	}
	d.serve(p)
}
func (d *Service) admit(stream *host.Stream) (*session, error) {
	c := stream.Conn()
	peer := c.PeerID()
	v, ok := d.cfg.Socket.PeerAddr(peer)
	a, udp := c.RemoteAddr().(*net.UDPAddr)
	if !ok || !udp || netip.AddrPortFrom(a.AddrPort().Addr().Unmap(), a.AddrPort().Port()) != v {
		return nil, errors.New("disco: not an admitted inner connection")
	}
	now := time.Now()
	s := &session{stream: stream, peer: peer, done: make(chan struct{}), remote: make(map[netip.AddrPort]Candidate), pending: make(map[[16]byte]challenge), proofs: make(map[netip.AddrPort]proof), updates: make(chan struct{}, 1), start: now, next: now, refresh: now.Add(2 * time.Minute)}
	if _, err := rand.Read(s.nonce[:]); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if len(d.sessions) >= 128 || d.sessions[peer] != nil {
		return nil, wire.ErrLimit
	}
	d.sessions[peer] = s
	d.wg.Add(1)
	return s, nil
}
func signal(s *session) {
	select {
	case s.updates <- struct{}{}:
	default:
	}
}
func (d *Service) serve(s *session) {
	defer d.wg.Done()
	defer s.stream.Reset()
	defer func() {
		d.mu.Lock()
		d.revoke(s)
		delete(d.sessions, s.peer)
		delete(d.ids, s.id)
		clear(s.tx)
		clear(s.rx)
		d.mu.Unlock()
	}()
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); d.writeUpdates(s); s.stream.Reset() }()
	defer func() { s.stream.Reset(); close(s.done); <-writerDone }()
	_ = s.stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	hello, err := readControl(s.stream)
	if err != nil || len(hello) != 18 || hello[0] != 1 || hello[1] != 0 {
		return
	}
	var remoteNonce [16]byte
	copy(remoteNonce[:], hello[2:])
	id, tx, rx, err := keys(s.stream.Conn(), d.cfg.Host.ID(), s.peer, s.nonce, remoteNonce)
	if err != nil {
		return
	}
	d.mu.Lock()
	if d.closed || d.ids[id] != nil {
		d.mu.Unlock()
		return
	}
	s.id = id
	s.tx = tx
	s.rx = rx
	s.ready = true
	d.ids[id] = s
	d.mu.Unlock()
	_ = s.stream.SetReadDeadline(time.Time{})
	for {
		b, err := readControl(s.stream)
		if err != nil {
			return
		}
		now := time.Now()
		gen, list, err := decodeControl(b, now)
		if err != nil {
			return
		}
		d.mu.Lock()
		if d.closed || s.dead || !s.in.take(now, 1, 4) || gen <= s.remoteGen {
			d.mu.Unlock()
			return
		}
		s.remoteGen = gen
		next := make(map[netip.AddrPort]Candidate)
		for _, c := range list {
			if d.cfg.Authorize(s.peer, c) {
				next[c.Addr] = c
			}
		}
		// A lease refresh alone doesn't cause periodic direct-path flapping.
		changed := len(next) != len(s.remote)
		for a, c := range next {
			if old, ok := s.remote[a]; !ok || old.Kind != c.Kind {
				changed = true
			}
		}
		s.remote = next
		d.prune(s, now)
		if changed {
			d.restartSchedule(s, now)
		}
		d.mu.Unlock()
	}
}
func (d *Service) writeUpdates(s *session) {
	_ = s.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	hello := make([]byte, 18)
	hello[0] = 1
	copy(hello[2:], s.nonce[:])
	if err := writeControl(s.stream, hello); err != nil {
		return
	}
	var last time.Time
	for {
		if !last.IsZero() {
			wait := time.Until(last.Add(time.Second))
			if wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-d.ctx.Done():
					t.Stop()
					return
				case <-s.done:
					t.Stop()
					return
				case <-s.stream.Conn().Context().Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
		list := d.cfg.Candidates(s.peer)
		now := time.Now()
		// Local providers may retain an expired lease; don't advertise it again.
		clean := make([]Candidate, 0, len(list))
		for _, c := range list {
			if c.Expires.Sub(now) >= time.Second {
				clean = append(clean, c)
			}
		}
		if s.localGen == math.MaxUint64 {
			return
		}
		s.localGen++
		b, err := encodeControl(s.localGen, clean, now)
		if err != nil {
			return
		}
		_ = s.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err = writeControl(s.stream, b); err != nil {
			return
		}
		last = time.Now()
		select {
		case <-d.ctx.Done():
			return
		case <-s.done:
			return
		case <-s.stream.Conn().Context().Done():
			return
		case <-s.updates:
		}
	}
}

// Update coalesces local candidate/mapping/profile invalidations and restarts the
// early schedule. Providers are reread by the control writer. Call on portmap
// Changes; do not send PCP PEER candidates to a different endpoint.
func (d *Service) Update() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	now := time.Now()
	for _, s := range d.sessions {
		signal(s)
		d.restartSchedule(s, now)
		d.prune(s, now)
	}
}

// RestartProbing is directly registerable at netmon.RestartProbing. Earlier
// pipeline stages must have refreshed the provider and surviving relay first.
func (d *Service) RestartProbing(ctx context.Context, _ netmon.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return net.ErrClosed
	}
	var errs []error
	for _, s := range d.sessions {
		d.revoke(s)
		if err := d.cfg.Socket.UseRelay(s.peer); err != nil {
			errs = append(errs, err)
		}
		d.restartSchedule(s, time.Now())
		signal(s)
	}
	return errors.Join(errs...)
}
func (d *Service) Status(peer wire.PeerID) (Status, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[peer]
	if s == nil {
		return Status{}, false
	}
	st := s.stats
	st.Selected = s.selected
	st.Candidates = len(s.remote)
	st.Validated = len(s.proofs)
	st.Pending = len(s.pending)
	if p, ok := s.proofs[s.selected]; ok {
		st.RTT = p.rtt
	}
	return st, true
}
func (d *Service) restartSchedule(s *session, now time.Time) { s.round = 0; s.retry = 0; s.next = now }
func (d *Service) revoke(s *session) {
	if s.selected.IsValid() {
		s.stats.Fallbacks++
	}
	s.selected = netip.AddrPort{}
	clear(s.proofs)
	clear(s.pending)
	// SetDirectPaths clears selection even if the relay is temporarily unavailable.
	_ = d.cfg.Socket.SetDirectPaths(s.peer, nil)
	_ = d.cfg.Socket.UseRelay(s.peer)
}
func (d *Service) syncPaths(s *session) {
	paths := make([]netip.AddrPort, 0, len(s.proofs))
	for a := range s.proofs {
		paths = append(paths, a)
	}
	_ = d.cfg.Socket.SetDirectPaths(s.peer, paths)
}
func (d *Service) prune(s *session, now time.Time) {
	changed := false
	for a, c := range s.remote {
		if !now.Before(c.Expires) || !d.cfg.Authorize(s.peer, c) {
			delete(s.remote, a)
		}
	}
	for a := range s.proofs {
		if _, ok := s.remote[a]; !ok {
			delete(s.proofs, a)
			changed = true
		}
	}
	for nonce, p := range s.pending {
		if _, ok := s.remote[p.addr]; !ok {
			delete(s.pending, nonce)
			continue
		}
		if now.Sub(p.sent) >= d.cfg.ProbeTimeout {
			delete(s.pending, nonce)
			if proof, ok := s.proofs[p.addr]; ok && !proof.last.After(p.sent) {
				delete(s.proofs, p.addr)
				changed = true
			}
		}
	}
	if changed {
		d.syncPaths(s)
	}
	if s.selected.IsValid() {
		if _, ok := s.proofs[s.selected]; !ok {
			d.revoke(s)
			d.restartSchedule(s, now)
		}
	}
}

var early = [...]time.Duration{0, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
var retries = [...]time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

func (d *Service) run() {
	defer d.wg.Done()
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	defer func() {
		d.mu.Lock()
		for _, s := range d.sessions {
			s.stream.Reset()
		}
		d.mu.Unlock()
	}()
	for {
		select {
		case <-d.ctx.Done():
			return
		case now := <-t.C:
			d.mu.Lock()
			for _, s := range d.sessions {
				if s.dead || !s.ready {
					continue
				}
				d.prune(s, now)
				if !now.Before(s.refresh) {
					signal(s)
					s.refresh = now.Add(2 * time.Minute)
				}
				if s.selected.IsValid() {
					for a, p := range s.proofs {
						if now.Sub(p.last) >= d.cfg.Heartbeat && !hasPending(s, a) {
							d.ping(s, a, now)
						}
					}
				} else if !now.Before(s.next) {
					list := make([]Candidate, 0, len(s.remote))
					for _, c := range s.remote {
						list = append(list, c)
					}
					sort.Slice(list, func(i, j int) bool {
						if list[i].Kind != list[j].Kind {
							return list[i].Kind < list[j].Kind
						}
						return list[i].Addr.Compare(list[j].Addr) < 0
					})
					for _, c := range list {
						d.ping(s, c.Addr, now)
					}
					advanceSchedule(s, now)
				}
			}
			d.mu.Unlock()
		}
	}
}

// Each retry starts another short simultaneous-probe wave, indefinitely. Times
// are relative to actual sends so a suspended scheduler doesn't emit a backlog.
func advanceSchedule(s *session, now time.Time) {
	if s.round < len(early)-1 {
		s.next = now.Add(early[s.round+1] - early[s.round])
		s.round++
		return
	}
	s.next = now.Add(retries[s.retry])
	if s.retry < len(retries)-1 {
		s.retry++
	}
	s.round = 0
}

func hasPending(s *session, a netip.AddrPort) bool {
	for _, p := range s.pending {
		if p.addr == a {
			return true
		}
	}
	return false
}
func (d *Service) send(s *session, p probe, a netip.AddrPort, now time.Time) bool {
	if s.seq == math.MaxUint64 {
		s.dead = true
		s.stream.Reset()
		d.revoke(s)
		return false
	}
	if !s.out.available(now, 20, 32) || !d.global.available(now, 100, 128) {
		return false
	}
	s.out.tokens--
	d.global.tokens--
	s.seq++
	p.seq = s.seq
	p.session = s.id
	if err := d.cfg.Socket.WriteDisco(encodeProbe(p, s.tx), a); err != nil {
		return false
	}
	s.stats.Sent++
	return true
}
func (d *Service) ping(s *session, a netip.AddrPort, now time.Time) {
	c, ok := s.remote[a]
	if !ok || !now.Before(c.Expires) || !d.cfg.Authorize(s.peer, c) {
		return
	}
	// At most two concurrent challenges per endpoint. Prefer fresh challenges in
	// the aggressive schedule without growing state or reusing nonces.
	count := 0
	var oldest [16]byte
	var at time.Time
	for n, p := range s.pending {
		if p.addr == a {
			count++
			if at.IsZero() || p.sent.Before(at) {
				oldest = n
				at = p.sent
			}
		}
	}
	if count >= 2 {
		delete(s.pending, oldest)
	}
	if len(s.pending) >= 32 {
		return
	}
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return
	}
	if d.send(s, probe{kind: 1, challenge: n}, a, now) {
		s.pending[n] = challenge{a, now}
	}
}
func (d *Service) receive() {
	defer d.wg.Done()
	b := make([]byte, magicsock.MaxDiscoPayload)
	for {
		n, from, err := d.cfg.Socket.ReadDisco(d.ctx, b)
		if err != nil {
			d.cancel()
			return
		}
		if n != probeSize {
			continue
		}
		var id [16]byte
		copy(id[:], b[2:18])
		d.mu.Lock()
		s := d.ids[id]
		if s != nil && !s.dead {
			d.handleProbe(s, b[:n], from, time.Now())
		}
		d.mu.Unlock()
	}
}
func (d *Service) handleProbe(s *session, b []byte, from netip.AddrPort, now time.Time) {
	c, ok := s.remote[from]
	if !ok || !now.Before(c.Expires) || !d.cfg.Authorize(s.peer, c) {
		s.stats.Rejected++
		return
	}
	p, err := decodeProbe(b, s.rx)
	if err != nil || p.session != s.id || !s.replay.accept(p.seq) {
		s.stats.Rejected++
		return
	}
	s.stats.Received++
	if p.kind == 1 {
		d.send(s, probe{kind: 2, challenge: p.challenge, observed: from}, from, now)
		return
	}
	pending, ok := s.pending[p.challenge]
	if !ok || pending.addr != from || now.Sub(pending.sent) >= d.cfg.ProbeTimeout {
		s.stats.Rejected++
		return
	}
	delete(s.pending, p.challenge)
	rtt := now.Sub(pending.sent)
	if rtt <= 0 {
		rtt = time.Nanosecond
	}
	if old, ok := s.proofs[from]; ok {
		rtt = (old.rtt*3 + rtt) / 4
	}
	s.proofs[from] = proof{now, rtt}
	s.stats.Observed = p.observed
	d.syncPaths(s)
	switchPath := !s.selected.IsValid()
	if old, ok := s.proofs[s.selected]; ok && from != s.selected && now.Sub(s.selectedAt) >= 5*time.Second && rtt*5 < old.rtt*4 && old.rtt-rtt > 5*time.Millisecond {
		switchPath = true
	}
	if switchPath && d.cfg.Socket.UseDirect(s.peer, from) == nil {
		s.selected = from
		s.selectedAt = now
		if s.stats.TimeToDirect == 0 {
			s.stats.TimeToDirect = now.Sub(s.start)
		}
	}
}
