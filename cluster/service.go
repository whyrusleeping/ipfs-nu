package cluster

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// DefaultInterval is the heartbeat and anti-entropy interval.
const DefaultInterval = 30 * time.Second

// ServiceConfig configures the networking of a networked cluster.
type ServiceConfig struct {
	Cluster *Cluster
	Host    *host.Host
	Key     *wire.NodeKey
	// Connect dials a peer at addrs (the swarm's session layer).
	Connect func(ctx context.Context, id wire.PeerID, addrs []wire.Multiaddr) error
	// Addrs are this node's advertised addresses (for heartbeats and join requests).
	Addrs func() []wire.Multiaddr
	// Usage reports free and used bytes for heartbeats; nil reports zeros.
	Usage    func() (free, used int64)
	Interval time.Duration // 0: DefaultInterval
	Logger   *slog.Logger
	// Holdings serves `holdings` requests (collections, D22); nil answers not found.
	Holdings func(peer wire.PeerID, req HoldingsRequest) (*HoldingsPage, error)
	// HoldingsMarks reports this node's holdings-log positions for heartbeats.
	HoldingsMarks func() []HoldingsMark
	// Reports returns this node's current completion reports (seq unset; the service
	// numbers changes) for heartbeats; nil sends none.
	Reports func() []Report
}

// PeerStatus is what this node knows about another member.
type PeerStatus struct {
	Heartbeat *Heartbeat
	Received  time.Time
	// Skew is the peer's clock minus ours, measured at heartbeat receipt.
	Skew  time.Duration
	Heads map[string]string // did → rev last reported by the peer
	// Reports are the peer's completion reports by dataset at report sequence RSeq, all
	// confirmed current at ReportsAt (this node's clock).
	Reports   map[string]Report
	RSeq      int64
	ReportsAt time.Time
	pulling   bool
}

// Service runs a networked cluster on a host: gating, state replication (heads
// exchange, pulls, pushes to connected members), heartbeats and the join exchange.
type Service struct {
	cfg  ServiceConfig
	c    *Cluster
	h    *host.Host
	self wire.PeerID
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   chan struct{}

	mu      sync.Mutex
	peers   map[wire.PeerID]*PeerStatus
	seq     int64
	pulling map[string]bool
	closed  bool
	changed chan struct{} // closed and replaced when peer heads change

	hbKick   chan struct{}
	own      map[string]Report // this node's reports with their seqs
	ownSeq   int64             // report sequence: the greatest seq assigned
	sentSeq  int64             // ownSeq at the previous heartbeat (the next delta's base)
	sentOnce bool
	evidence chan struct{}            // closed and replaced when received reports change
	learned  map[wire.PeerID][]string // member addresses relayed by other members' heartbeats
}

// Gate is the connection policy of c for this node (self): in private and isolated mode
// only non-removed members, the boot peers while joining, and, while a secret invite is
// live, anyone (whose streams the stream gate then confines to the join exchange).
// Install it before the host listens.
func Gate(c *Cluster, self wire.PeerID) host.ConnectionGater {
	if c.Mode() != ModePrivate && c.Mode() != ModeIsolated {
		return nil
	}
	return func(id wire.PeerID) bool {
		v := c.View()
		return id == self || v.IsMember(id) || bootAllowed(c, v, self, id) || v.HasLiveInvites(c.Now())
	}
}

// DialGate is the outbound policy: members, and boot peers while joining.
func DialGate(c *Cluster, self wire.PeerID) host.ConnectionGater {
	if c.Mode() != ModePrivate && c.Mode() != ModeIsolated {
		return nil
	}
	return func(id wire.PeerID) bool {
		v := c.View()
		return v.IsMember(id) || bootAllowed(c, v, self, id)
	}
}

func bootAllowed(c *Cluster, v *View, self, id wire.PeerID) bool {
	if v.IsMember(self) {
		return false
	}
	for _, b := range c.Boot() {
		if b.Peer == id {
			return true
		}
	}
	return false
}

// StartService registers /nu/cluster/1, installs the stream and dial gates and starts
// replication and heartbeats. The connection gate (Gate) must already be in place.
func StartService(cfg ServiceConfig) (*Service, error) {
	if cfg.Cluster == nil || cfg.Host == nil || cfg.Key == nil {
		return nil, errors.New("cluster: service needs a cluster, host and key")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Service{cfg: cfg, c: cfg.Cluster, h: cfg.Host, self: cfg.Host.ID(), log: cfg.Logger.With("cluster", cfg.Cluster.ID.String()),
		kick: make(chan struct{}, 1), peers: map[wire.PeerID]*PeerStatus{}, pulling: map[string]bool{}, changed: make(chan struct{}),
		hbKick: make(chan struct{}, 1), own: map[string]Report{}, evidence: make(chan struct{}), learned: map[wire.PeerID][]string{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	private := s.c.Mode() == ModePrivate || s.c.Mode() == ModeIsolated
	if private {
		s.h.SetDialGater(DialGate(s.c, s.self))
		s.h.SetStreamGate(func(peer wire.PeerID, protocol string) bool {
			v := s.c.View()
			return v.IsMember(peer) || bootAllowed(s.c, v, s.self, peer) || protocol == ProtocolID
		})
	}
	if err := s.h.Handle(ProtocolID, 64, s.serve); err != nil {
		return nil, err
	}
	s.c.OnChange(func() {
		if private {
			s.h.RecheckConnections()
		}
		select {
		case s.kick <- struct{}{}:
		default:
		}
	})
	s.spawn(s.run)
	return s, nil
}

// spawn runs fn in a goroutine Close waits for, unless the service is closing.
func (s *Service) spawn(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// Close stops the service. Gates stay installed (a private host never opens up).
func (s *Service) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.h.RemoveHandler(ProtocolID)
	s.wg.Wait()
	return nil
}

func (s *Service) run() {
	changes, stop := s.h.WatchChanges()
	defer stop()
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	connected := map[wire.PeerID]bool{}
	s.connectMembers()
	s.heartbeat()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-changes:
			// Exchange heads with members that just connected.
			now := map[wire.PeerID]bool{}
			for _, c := range s.h.Connections() {
				id := c.PeerID()
				now[id] = true
				if !connected[id] && s.trusted(s.c.View(), id) {
					s.exchange(id)
				}
			}
			connected = now
		case <-s.kick:
			for _, id := range s.connectedMembers() {
				s.exchange(id)
			}
		case <-s.hbKick:
			s.heartbeat()
		case <-tick.C:
			s.c.Refresh()
			s.connectMembers()
			for _, id := range s.connectedMembers() {
				s.exchange(id)
			}
			s.heartbeat()
		}
	}
}

func (s *Service) connectedMembers() []wire.PeerID {
	v := s.c.View()
	var out []wire.PeerID
	seen := map[wire.PeerID]bool{}
	for _, c := range s.h.Connections() {
		id := c.PeerID()
		if !seen[id] && id != s.self && s.trusted(v, id) {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// trusted reports whether a peer may use the full protocol: a member, or a boot peer
// while this node is joining.
func (s *Service) trusted(v *View, id wire.PeerID) bool {
	return v.IsMember(id) || bootAllowed(s.c, v, s.self, id)
}

// addrsOf collects the known addresses of a member: its record, its last heartbeat and
// the join token's boot entry.
func (s *Service) addrsOf(id wire.PeerID) []wire.Multiaddr {
	var texts []string
	if m, ok := s.c.View().Members[id]; ok && m.Value != nil {
		texts = append(texts, m.Value.Addrs...)
	}
	s.mu.Lock()
	if p := s.peers[id]; p != nil && p.Heartbeat != nil {
		texts = append(texts, p.Heartbeat.Addrs...)
	}
	texts = append(texts, s.learned[id]...)
	s.mu.Unlock()
	for _, b := range s.c.Boot() {
		if b.Peer == id {
			texts = append(texts, b.Addrs...)
		}
	}
	var out []wire.Multiaddr
	seen := map[string]bool{}
	for _, t := range texts {
		if seen[t] {
			continue
		}
		seen[t] = true
		if a, err := wire.ParseMultiaddr(t, 1024); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// connectMembers dials members (and boot peers while joining) that are not connected.
func (s *Service) connectMembers() {
	if s.cfg.Connect == nil {
		return
	}
	v := s.c.View()
	targets := v.MemberIDs()
	if !v.IsMember(s.self) {
		for _, b := range s.c.Boot() {
			targets = append(targets, b.Peer)
		}
	}
	for _, id := range targets {
		if id == s.self {
			continue
		}
		if _, ok := s.h.Connection(id); ok {
			continue
		}
		addrs := s.addrsOf(id)
		if len(addrs) == 0 {
			continue
		}
		s.spawn(func() {
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			defer cancel()
			if err := s.cfg.Connect(ctx, id, addrs); err != nil {
				s.log.Debug("cannot reach member", "peer", id, "err", err)
			}
		})
	}
}

// exchange sends heads to a peer and pulls what it has newer (asynchronously).
func (s *Service) exchange(id wire.PeerID) {
	s.spawn(func() {
		res, err := call(s.ctx, s.h, id, &request{Op: OpHeads, Cluster: s.c.ID, Heads: headsWire(s.c.Heads())}, nil)
		if err != nil {
			s.log.Debug("heads exchange failed", "peer", id, "err", err)
			return
		}
		s.sync(id, headsFromWire(res.Heads))
	})
}

// sync records a peer's heads and pulls every wanted repository from it, repeating while
// pulls unlock more (authorization grows as key records arrive).
func (s *Service) sync(id wire.PeerID, heads []HeadInfo) {
	s.noteHeads(id, heads)
	for range 8 {
		wants := s.c.Wants(heads)
		progressed := false
		for did, at := range wants {
			if s.pull(id, did, at) {
				progressed = true
			}
		}
		if !progressed {
			return
		}
	}
}

func (s *Service) pull(id wire.PeerID, did string, at cid.CID) bool {
	s.mu.Lock()
	if s.pulling[did] {
		s.mu.Unlock()
		return false
	}
	s.pulling[did] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pulling, did)
		s.mu.Unlock()
	}()
	var changed bool
	_, err := call(s.ctx, s.h, id, &request{Op: OpPull, Cluster: s.c.ID, DID: did, Since: s.c.SinceFor(did), Commit: at}, func(r io.Reader) error {
		var err error
		changed, err = s.c.Apply(did, at, r)
		return err
	})
	if err != nil && !errors.Is(err, ErrStale) {
		s.log.Debug("pull failed", "peer", id, "did", did, "err", err)
	}
	return changed
}

func (s *Service) noteHeads(id wire.PeerID, heads []HeadInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[id]
	if p == nil {
		p = &PeerStatus{}
		s.peers[id] = p
	}
	p.Heads = map[string]string{}
	for _, h := range heads {
		p.Heads[h.DID] = h.Rev
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// heartbeat sends a signed heartbeat to every connected member.
func (s *Service) heartbeat() {
	var free, used int64
	if s.cfg.Usage != nil {
		free, used = s.cfg.Usage()
	}
	s.mu.Lock()
	s.seq++
	hb := Heartbeat{Peer: s.self.Bytes(), Seq: s.seq, Time: s.c.Now().UnixMilli(), Free: free, Used: used}
	s.mu.Unlock()
	if s.cfg.Reports != nil {
		hb.Reports = s.reportDelta(s.cfg.Reports())
	}
	hb.Peers = s.relayAddrs()
	if s.cfg.HoldingsMarks != nil {
		hb.Holdings = s.cfg.HoldingsMarks()
	}
	if s.cfg.Addrs != nil {
		for _, a := range s.cfg.Addrs() {
			if len(hb.Addrs) < 16 {
				hb.Addrs = append(hb.Addrs, a.String())
			}
		}
	}
	if err := hb.Sign(s.cfg.Key); err != nil {
		return
	}
	for _, id := range s.connectedMembers() {
		s.spawn(func() {
			if _, err := call(s.ctx, s.h, id, &request{Op: OpHeartbeat, Cluster: s.c.ID, HB: &hb}, nil); err != nil {
				s.log.Debug("heartbeat failed", "peer", id, "err", err)
			}
		})
	}
}

// Peers returns what is known about other members.
func (s *Service) Peers() map[wire.PeerID]PeerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[wire.PeerID]PeerStatus{}
	for id, p := range s.peers {
		out[id] = *p
	}
	return out
}

// Connected reports whether a member is connected.
func (s *Service) Connected(id wire.PeerID) bool {
	_, ok := s.h.Connection(id)
	return ok
}

// Acked lists the storage members (including this node when it is one) known to hold
// did at rev or later.
func (s *Service) Acked(did, rev string) []wire.PeerID {
	v := s.c.View()
	storage := func(id wire.PeerID) bool {
		m, ok := v.Members[id]
		return ok && !m.Deleted && m.Value.State != StateRemoved && slices.Contains(m.Value.Roles, RoleStorage)
	}
	var out []wire.PeerID
	if h, ok := s.c.Held(did); ok && storage(s.self) && !revLess(h.Rev, rev) {
		out = append(out, s.self)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if r, ok := p.Heads[did]; ok && storage(id) && !revLess(r, rev) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// WaitAcked waits until n storage members hold did at rev, or ctx ends; it returns the
// members known to hold it either way.
func (s *Service) WaitAcked(ctx context.Context, did, rev string, n int) []wire.PeerID {
	for {
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		got := s.Acked(did, rev)
		if len(got) >= n {
			return got
		}
		select {
		case <-ctx.Done():
			return got
		case <-ch:
		}
	}
}

// Holdings asks a member for its item holdings of a collection.
func (s *Service) Holdings(ctx context.Context, peer wire.PeerID, req HoldingsRequest) (*HoldingsPage, error) {
	if req.Limit <= 0 || req.Limit > MaxHoldingsPage {
		req.Limit = MaxHoldingsPage
	}
	var page HoldingsPage
	_, err := call(ctx, s.h, peer, &request{Op: OpHoldings, Cluster: s.c.ID, Dataset: req.Dataset, Epoch: req.Epoch, SinceSeq: req.Since,
		Snapshot: req.Snapshot, After: req.After, Limit: int64(req.Limit)}, func(r io.Reader) error {
		b, err := wire.ReadMessage(bufio.NewReader(r), MaxHoldingsBody)
		if err != nil {
			return err
		}
		return (drisl.DecOptions{MaxSize: MaxHoldingsBody, MaxDepth: 8}).Unmarshal(b, &page)
	})
	if err != nil {
		return nil, err
	}
	if len(page.Entries) > MaxHoldingsPage {
		return nil, errors.New("cluster: holdings page too long")
	}
	return &page, nil
}

// Join performs the secret-invite join exchange with the token's boot peers. A
// pre-authorized token needs no exchange; replication starts once a boot peer accepts
// the connection.
func (s *Service) Join(ctx context.Context, t *Token) error {
	if t.Secret == nil {
		s.connectMembers()
		return nil
	}
	proof := JoinProof(t.Secret, s.c.ID, s.self)
	var addrs []string
	if s.cfg.Addrs != nil {
		for _, a := range s.cfg.Addrs() {
			if len(addrs) < 16 {
				addrs = append(addrs, a.String())
			}
		}
	}
	// Each round tries every boot peer with its own deadline (a dial stalled by load or
	// a lost handshake does not use up ctx), and rounds repeat until ctx ends unless every
	// peer answered with a refusal. Admission is idempotent for a joiner already admitted.
	attempt, backoff := 3*time.Second, 100*time.Millisecond
	for {
		var errs []error
		refused := true
		for _, b := range t.Boot {
			actx, cancel := context.WithTimeout(ctx, attempt)
			err := s.joinVia(actx, b, &request{Op: OpJoin, Cluster: s.c.ID, Invite: t.InviteID, Proof: proof, Addrs: addrs})
			cancel()
			if err == nil {
				s.exchange(b.Peer)
				return nil
			}
			var se *StatusError
			refused = refused && errors.As(err, &se)
			errs = append(errs, fmt.Errorf("%s: %w", b.Peer, err))
		}
		if refused || ctx.Err() != nil {
			return errors.Join(errs...)
		}
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		case <-time.After(backoff):
		}
		attempt, backoff = min(2*attempt, 20*time.Second), min(2*backoff, 2*time.Second)
	}
}

// joinVia connects to boot peer b and sends the join request.
func (s *Service) joinVia(ctx context.Context, b Boot, req *request) error {
	var maddrs []wire.Multiaddr
	for _, a := range b.Addrs {
		if m, err := wire.ParseMultiaddr(a, 1024); err == nil {
			maddrs = append(maddrs, m)
		}
	}
	if s.cfg.Connect != nil {
		if err := s.cfg.Connect(ctx, b.Peer, maddrs); err != nil {
			return err
		}
	}
	_, err := call(ctx, s.h, b.Peer, req, nil)
	return err
}

// WaitSynced waits until this node sees itself as a member (the first sync after
// joining), or ctx ends.
func (s *Service) WaitSynced(ctx context.Context) error {
	for !s.c.View().IsMember(s.self) {
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-time.After(50 * time.Millisecond):
			for _, b := range s.c.Boot() {
				if s.Connected(b.Peer) {
					s.exchange(b.Peer)
				}
			}
		}
	}
	return nil
}

// Server side.

func (s *Service) serve(st *host.Stream) {
	peer := st.Conn().PeerID()
	_ = st.SetDeadline(time.Now().Add(30 * time.Second))
	b, err := wire.ReadMessage(st, MaxMessage)
	if err != nil {
		st.Reset()
		return
	}
	reply := func(res response, body []byte) {
		rb, err := drisl.Marshal(res)
		if err == nil {
			err = wire.WriteMessage(st, rb, MaxMessage)
		}
		if err == nil && body != nil {
			_, err = st.Write(body)
		}
		if err != nil {
			st.Reset()
		}
	}
	refuse := func(status int64, format string, args ...any) {
		reply(response{Status: status, Error: fmt.Sprintf(format, args...)}, nil)
	}
	req, err := parseRequest(b)
	if err != nil {
		refuse(StatusBadRequest, "malformed request: %v", err)
		return
	}
	if req.Cluster != s.c.ID {
		refuse(StatusBadRequest, "not a member of that cluster")
		return
	}
	v := s.c.View()
	member := s.trusted(v, peer)
	if !member && req.Op != OpJoin {
		refuse(StatusForbidden, "not a member")
		return
	}
	switch req.Op {
	case OpHeads:
		heads := headsFromWire(req.Heads)
		reply(response{Heads: headsWire(s.c.Heads())}, nil)
		s.spawn(func() { s.sync(peer, heads) })
	case OpPull:
		var buf bytes.Buffer
		if err := s.c.WriteDiff(&buf, req.DID, req.Since, req.Commit); err != nil {
			refuse(StatusNotFound, "%v", err)
			return
		}
		reply(response{}, buf.Bytes())
	case OpHeartbeat:
		if req.HB == nil || req.HB.Verify(st.Conn().PublicKey()) != nil {
			refuse(StatusBadRequest, "heartbeat signature does not verify")
			return
		}
		s.mu.Lock()
		p := s.peers[peer]
		if p == nil {
			p = &PeerStatus{}
			s.peers[peer] = p
		}
		pull := false
		if p.Heartbeat == nil || req.HB.Seq > p.Heartbeat.Seq || req.HB.Seq == 1 {
			now := s.c.Now()
			p.Heartbeat, p.Received = req.HB, now
			p.Skew = time.UnixMilli(req.HB.Time).Sub(now)
			if d := req.HB.Reports; d != nil {
				pull = s.applyReportsLocked(p, d, now)
			}
			for _, pa := range req.HB.Peers {
				if id, err := wire.PeerIDFromBytes(pa.Peer); err == nil && id != s.self && len(pa.Addrs) <= 16 {
					s.learned[id] = pa.Addrs
				}
			}
		}
		s.mu.Unlock()
		reply(response{}, nil)
		if pull {
			s.spawn(func() { s.pullReports(peer) })
		}

	case OpReports:
		body, err := s.serveReports()
		if err != nil {
			refuse(StatusInternal, "%v", err)
			return
		}
		reply(response{}, body)
	case OpHoldings:
		if s.cfg.Holdings == nil {
			refuse(StatusNotFound, "this member keeps no holdings logs")
			return
		}
		page, err := s.cfg.Holdings(peer, HoldingsRequest{Dataset: req.Dataset, Epoch: req.Epoch, Since: req.SinceSeq, Snapshot: req.Snapshot, After: req.After, Limit: int(req.Limit)})
		if err != nil {
			refuse(StatusNotFound, "%v", err)
			return
		}
		body, err := drisl.Marshal(page)
		if err == nil && len(body) > MaxHoldingsBody {
			err = errors.New("holdings page too large")
		}
		if err != nil {
			refuse(StatusInternal, "%v", err)
			return
		}
		var framed bytes.Buffer
		if err := wire.WriteMessage(&framed, body, MaxHoldingsBody); err != nil {
			refuse(StatusInternal, "%v", err)
			return
		}
		reply(response{}, framed.Bytes())
	case OpJoin, OpAdmit:
		joiner := peer
		if req.Op == OpAdmit {
			id, err := wire.PeerIDFromBytes(req.Peer)
			if err != nil {
				refuse(StatusBadRequest, "bad peer")
				return
			}
			joiner = id
		}
		status, msg := s.admit(joiner, req.Invite, req.Proof, req.Addrs, req.Op == OpJoin)
		if status != StatusOK {
			refuse(status, "%s", msg)
		} else {
			reply(response{}, nil)
		}
		if !member && req.Op == OpJoin && status != StatusOK {
			// A refused non-member gets no more than this exchange.
			time.AfterFunc(2*time.Second, func() {
				if !s.c.View().IsMember(peer) {
					s.h.Disconnect(peer)
				}
			})
		}
	default:
		refuse(StatusBadRequest, "unknown op %q", req.Op)
	}
}

// admit checks a join proof and writes the member record, or forwards it to a member
// holding an admin key.
func (s *Service) admit(joiner wire.PeerID, invite string, proof []byte, addrs []string, forward bool) (int64, string) {
	v := s.c.View()
	if v.IsMember(joiner) {
		return StatusOK, ""
	}
	in, ok := v.LiveInvite(invite, s.c.Now())
	if !ok {
		return StatusNotFound, "the invite is unknown here, already used, revoked or expired"
	}
	if !VerifyJoinProof(in.ProofKey, s.c.ID, joiner, proof) {
		return StatusForbidden, "the invite proof does not verify for this PeerId"
	}
	var clean []string
	for _, a := range addrs {
		if _, err := wire.ParseMultiaddr(a, 1024); err == nil && len(clean) < 16 {
			clean = append(clean, a)
		}
	}
	if s.c.HoldsKey(RoleAdmin) {
		_, err := s.c.Write(
			Op{Collection: NSMember, RKey: joiner.String(), Value: &Member{Roles: in.Roles, Capacity: in.Capacity, Tags: in.Tags, Addrs: clean, State: StateActive}},
			Op{Collection: NSInvite, RKey: invite, Delete: true})
		if err != nil {
			return StatusInternal, err.Error()
		}
		return StatusOK, ""
	}
	if !forward {
		return StatusForbidden, "this member holds no admin key"
	}
	for _, id := range s.connectedMembers() {
		if id == joiner {
			continue
		}
		_, err := call(s.ctx, s.h, id, &request{Op: OpAdmit, Cluster: s.c.ID, Invite: invite, Proof: proof, Peer: joiner.Bytes(), Addrs: clean}, nil)
		if err != nil {
			continue
		}
		// Answer once this node has merged the admission, so the joiner can sync here.
		ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		for !s.c.View().IsMember(joiner) && ctx.Err() == nil {
			s.exchange(id)
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
		cancel()
		return StatusOK, ""
	}
	return StatusNotFound, "no connected member holding an admin key accepted the invite"
}

// relayAddrs lists the heartbeat addresses (at most 4 each) of connected members (at most
// 32), so members
// that are not connected to each other, such as two pre-authorized members whose records
// carry no addresses, can dial each other. Gating still admits only members, and the TLS
// handshake authenticates the PeerId.
func (s *Service) relayAddrs() []PeerAddrs {
	v := s.c.View()
	var out []PeerAddrs
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if len(out) == 32 || p.Heartbeat == nil || len(p.Heartbeat.Addrs) == 0 || !v.IsMember(id) {
			continue
		}
		if _, ok := s.h.Connection(id); ok {
			out = append(out, PeerAddrs{Peer: id.Bytes(), Addrs: p.Heartbeat.Addrs[:min(len(p.Heartbeat.Addrs), 4)]})
		}
	}
	slices.SortFunc(out, func(a, b PeerAddrs) int { return cmpStr(string(a.Peer), string(b.Peer)) })
	return out
}
