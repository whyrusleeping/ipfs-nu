package proto

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	nu "github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type Config struct {
	AgentVersion, ProtocolVersion string
	MaxPeers, MaxObservers        int
	Timeout                       time.Duration
}
type PeerInfo struct {
	Message   IdentifyMessage
	Certified *wire.PeerRecord
	Updated   time.Time
}

// Observation is an authenticated peer's claim, NOT a reachability proof. The
// observer key is its actual connection IP/port, not its advertised listen addr.
type Observation struct {
	ObserverEndpoint      string
	Observer              wire.PeerID
	Address, LocalAddress wire.Multiaddr
	Updated               time.Time
}
type Service struct {
	host         *host.Host
	config       Config
	ctx          context.Context
	cancel       context.CancelFunc
	unwatch      func()
	wg           sync.WaitGroup
	closeOnce    sync.Once
	requests     chan struct{}
	mu           sync.Mutex
	peers        map[wire.PeerID]PeerInfo
	observations map[string]Observation
	changed      chan struct{}
	seq          uint64
}

func New(h *host.Host, cfg Config) (*Service, error) {
	if h == nil || h.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	for _, p := range h.Protocols() {
		if p == IdentifyID || p == IdentifyPushID || p == PingID {
			return nil, errors.New("proto: core service already registered")
		}
	}
	if cfg.MaxPeers < 0 || cfg.MaxObservers < 0 || cfg.Timeout < 0 {
		return nil, wire.ErrLimit
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 256
	}
	if cfg.MaxObservers == 0 {
		cfg.MaxObservers = 128
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = nu.Name + "/0.0.0"
	}
	if cfg.ProtocolVersion == "" {
		cfg.ProtocolVersion = "ipfs/0.1.0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{host: h, config: cfg, ctx: ctx, cancel: cancel, requests: make(chan struct{}, 32), peers: make(map[wire.PeerID]PeerInfo), observations: make(map[string]Observation), changed: make(chan struct{})}
	handlers := []struct {
		id string
		fn host.Handler
	}{{IdentifyID, s.identifyHandler}, {IdentifyPushID, s.pushHandler}, {PingID, s.pingHandler}}
	for i, handler := range handlers {
		if err := h.Handle(handler.id, 32, handler.fn); err != nil {
			for _, previous := range handlers[:i] {
				h.RemoveHandler(previous.id)
			}
			cancel()
			return nil, err
		}
	}
	changes, unwatch := h.WatchChanges()
	s.unwatch = unwatch
	s.wg.Add(1)
	go s.watch(changes)
	return s, nil
}
func recordLimits() wire.RecordLimits {
	return wire.RecordLimits{MaxBytes: 8 << 10, MaxAddresses: 128, MaxAddressBytes: 1024}
}
func (s *Service) ownMessage(c *host.Conn) (IdentifyMessage, error) {
	observed, err := host.UDPAddress(c.RemoteAddr())
	if err != nil {
		return IdentifyMessage{}, err
	}
	addrs := s.host.Addrs()
	s.mu.Lock()
	now := uint64(time.Now().UnixNano())
	if now > s.seq {
		s.seq = now
	} else {
		s.seq++
	}
	seq := s.seq
	s.mu.Unlock()
	record, err := s.host.SealPeerRecord(seq, addrs, recordLimits())
	if err != nil {
		return IdentifyMessage{}, err
	}
	var observedAddress *wire.Multiaddr
	if !s.host.IsLocalOnlyAddress(c.RemoteAddr()) {
		observedAddress = &observed
	}
	return IdentifyMessage{PublicKey: s.host.PublicKey().Marshal(), ListenAddrs: addrs, Protocols: s.host.Protocols(), ObservedAddr: observedAddress, ProtocolVersion: s.config.ProtocolVersion, AgentVersion: s.config.AgentVersion, SignedPeerRecord: record}, nil
}
func (s *Service) writeIdentify(stream *host.Stream) error {
	m, err := s.ownMessage(stream.Conn())
	if err != nil {
		return err
	}
	limits := DefaultIdentifyLimits()
	limits.MaxBytes = 4 << 10
	b, err := m.Marshal(limits)
	if err != nil {
		return err
	}
	return wire.WriteMessage(stream, b, 4<<10)
}
func (s *Service) identifyHandler(stream *host.Stream) {
	if s.ctx.Err() != nil {
		stream.Reset()
		return
	}
	_ = stream.SetDeadline(time.Now().Add(s.config.Timeout))
	if err := s.writeIdentify(stream); err != nil {
		stream.Reset()
	}
}
func (s *Service) pushHandler(stream *host.Stream) {
	if s.ctx.Err() != nil {
		stream.Reset()
		return
	}
	_ = stream.SetDeadline(time.Now().Add(s.config.Timeout))
	m, err := ReadIdentify(stream, DefaultIdentifyLimits())
	if err == nil {
		err = s.consume(stream.Conn(), m)
	}
	if err != nil {
		stream.Reset()
	}
}
func (s *Service) open(ctx context.Context, id wire.PeerID, p string, lazy bool) (*host.Stream, func(), error) {
	if err := s.ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case s.requests <- struct{}{}:
	default:
		return nil, nil, host.ErrLimit
	}
	opctx, cancel := context.WithCancel(ctx)
	stopService := context.AfterFunc(s.ctx, cancel)
	var stream *host.Stream
	var err error
	if lazy {
		stream, err = s.host.OpenStreamLazy(opctx, id, p)
	} else {
		stream, err = s.host.OpenStream(opctx, id, p)
	}
	if err != nil {
		stopService()
		cancel()
		<-s.requests
		return nil, nil, err
	}
	deadline := time.Now().Add(s.config.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)
	stopIO := context.AfterFunc(opctx, stream.Reset)
	cleanup := func() { stopIO(); stopService(); cancel(); _ = stream.Close(); <-s.requests }
	return stream, cleanup, nil
}
func (s *Service) Identify(ctx context.Context, id wire.PeerID) (PeerInfo, error) {
	stream, cleanup, err := s.open(ctx, id, IdentifyID, true)
	if err != nil {
		return PeerInfo{}, err
	}
	defer cleanup()
	m, err := ReadIdentify(stream, DefaultIdentifyLimits())
	if err != nil {
		stream.Reset()
		return PeerInfo{}, err
	}
	if err := s.consume(stream.Conn(), m); err != nil {
		stream.Reset()
		return PeerInfo{}, err
	}
	info, ok := s.Info(id)
	if !ok {
		return PeerInfo{}, errors.New("proto: peer information evicted")
	}
	return info, nil
}

// Push confirms protocol support and sends the full local snapshot. There is no
// application acknowledgement in identify-push; success is not a receipt proof.
func (s *Service) Push(ctx context.Context, id wire.PeerID) error {
	stream, cleanup, err := s.open(ctx, id, IdentifyPushID, true)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := s.writeIdentify(stream); err != nil {
		stream.Reset()
		return err
	}
	if err := stream.Confirm(); err != nil {
		stream.Reset()
		return err
	}
	return stream.CloseWrite()
}
func (s *Service) consume(c *host.Conn, m IdentifyMessage) error {
	id := c.PeerID()
	if len(m.PublicKey) > 0 {
		key, err := wire.ParsePublicKey(m.PublicKey, wire.MaxKeyBytes)
		if err != nil {
			return err
		}
		if key.PeerID() != id {
			return wire.ErrSignature
		}
	} else {
		m.PublicKey = c.PublicKey().Marshal()
	}
	var certified *wire.PeerRecord
	if len(m.SignedPeerRecord) > 0 {
		record, err := wire.OpenPeerRecord(m.SignedPeerRecord, recordLimits())
		if err != nil {
			return err
		}
		if record.PeerID != id {
			return wire.ErrSignature
		}
		certified = &record
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.peers[id]
	if !exists && len(s.peers) >= s.config.MaxPeers {
		var oldest wire.PeerID
		var when time.Time
		for peer, info := range s.peers {
			if oldest == "" || info.Updated.Before(when) {
				oldest = peer
				when = info.Updated
			}
		}
		delete(s.peers, oldest)
	}
	if m.AgentVersion == "" {
		m.AgentVersion = old.Message.AgentVersion
	}
	if m.ProtocolVersion == "" {
		m.ProtocolVersion = old.Message.ProtocolVersion
	}
	// Reject stale certified updates, including equal sequences, while allowing
	// repeated identify snapshots and unsigned protocol updates on this connection.
	if old.Certified != nil && (certified == nil || certified.Seq <= old.Certified.Seq) {
		certified = old.Certified
		m.SignedPeerRecord = old.Message.SignedPeerRecord
	}
	s.peers[id] = PeerInfo{Message: cloneMessage(m), Certified: certified, Updated: now}
	if m.ObservedAddr != nil && !s.host.IsLocalOnlyAddress(c.RemoteAddr()) {
		local, err := host.UDPAddress(c.LocalAddr())
		if err == nil {
			endpoint := c.RemoteAddr().String()
			if _, ok := s.observations[endpoint]; !ok && len(s.observations) >= s.config.MaxObservers {
				var oldest string
				var when time.Time
				for observer, obs := range s.observations {
					if oldest == "" || obs.Updated.Before(when) {
						oldest = observer
						when = obs.Updated
					}
				}
				delete(s.observations, oldest)
			}
			s.observations[endpoint] = Observation{ObserverEndpoint: endpoint, Observer: id, Address: *m.ObservedAddr, LocalAddress: local, Updated: now}
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}
func cloneInfo(info PeerInfo) PeerInfo {
	info.Message = cloneMessage(info.Message)
	if info.Certified != nil {
		record := *info.Certified
		record.Addresses = append([]wire.Multiaddr(nil), record.Addresses...)
		info.Certified = &record
	}
	return info
}
func (s *Service) Info(id wire.PeerID) (PeerInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.peers[id]
	return cloneInfo(info), ok
}
func (s *Service) Observations() []Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Observation, 0, len(s.observations))
	for _, obs := range s.observations {
		out = append(out, obs)
	}
	return out
}

// WaitPeer waits on updates without polling/sleeps. A nil predicate waits for
// any valid identify snapshot. Returned messages do not alias the service cache.
func (s *Service) WaitPeer(ctx context.Context, id wire.PeerID, ready func(PeerInfo) bool) (PeerInfo, error) {
	for {
		s.mu.Lock()
		info, ok := s.peers[id]
		info = cloneInfo(info)
		changed := s.changed
		s.mu.Unlock()
		if ok && (ready == nil || ready(info)) {
			return info, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return PeerInfo{}, ctx.Err()
		case <-s.ctx.Done():
			return PeerInfo{}, s.ctx.Err()
		case <-s.host.Context().Done():
			return PeerInfo{}, net.ErrClosed
		}
	}
}
func (s *Service) watch(changes <-chan struct{}) {
	defer s.wg.Done()
	known := make(map[*host.Conn]bool)
	for {
		select {
		case <-changes:
		case <-s.ctx.Done():
			return
		case <-s.host.Context().Done():
			return
		}
		current := make(map[*host.Conn]bool)
		for _, c := range s.host.Connections() {
			if s.ctx.Err() != nil || s.host.Context().Err() != nil {
				return
			}
			current[c] = true
			if !known[c] {
				_, _ = s.Identify(s.ctx, c.PeerID())
			}
			if info, ok := s.Info(c.PeerID()); ok && slices.Contains(info.Message.Protocols, IdentifyPushID) {
				_ = s.Push(s.ctx, c.PeerID())
			}
		}
		known = current
	}
}
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.unwatch()
		s.wg.Wait()
		s.host.RemoveHandler(IdentifyID)
		s.host.RemoveHandler(IdentifyPushID)
		s.host.RemoveHandler(PingID)
	})
	return nil
}
