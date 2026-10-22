package kad

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/varint"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const Protocol = "/ipfs/kad/1.0.0"
const ProviderTTL = 48 * time.Hour
const RepublishInterval = 22 * time.Hour

var ErrNoPeers = errors.New("kad: no Amino routing peers available")

type Config struct {
	Host                  *host.Host
	Sessions              *host.Sessions
	Private               bool
	PubliclyReachable     bool // set only from the owner's AutoNAT v2 decision
	MaxRequests           int
	MaxPerPeer            int
	MaxProviders          int
	ProviderTTL           time.Duration
	RequestTimeout        time.Duration
	Bootstrap             []Peer
	Advertise             []wire.Multiaddr
	AllowPrivateAddresses bool // explicit test/lab override; false for public Amino routing
	// ServerAddrs, if set, supplies the listen addresses of a peer that sent us a
	// request and advertises the Kademlia protocol (identify), so a server learns
	// routing peers from inbound traffic. Nil for peers that are clients only.
	ServerAddrs func(wire.PeerID) []wire.Multiaddr
}
type providerEntry struct {
	peers   []Peer
	expires map[wire.PeerID]time.Time
}
type Service struct {
	cfg             Config
	table           *Table
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	providers       map[string]providerEntry
	providerRecords int
	requests        chan struct{}
	peerRequests    map[wire.PeerID]int
	dialing         map[wire.PeerID]int // peers kad itself is connecting to (Lookup re-entrancy guard)
	closed          bool
	wg              sync.WaitGroup
}

// Stats is a snapshot for status/doctor.
type Stats struct {
	Server          bool
	RoutingPeers    int
	ProviderRecords int
}

func (s *Service) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Server: s.cfg.PubliclyReachable, RoutingPeers: len(s.table.Peers()), ProviderRecords: s.providerRecords}
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Private {
		return nil, errors.New("kad: disabled in private mode")
	}
	if cfg.Host == nil || cfg.Sessions == nil {
		return nil, wire.ErrFormat
	}
	if cfg.MaxRequests == 0 {
		cfg.MaxRequests = 32
	}
	if cfg.MaxPerPeer == 0 {
		cfg.MaxPerPeer = 4
	}
	if cfg.MaxProviders == 0 {
		cfg.MaxProviders = 4096
	}
	if cfg.ProviderTTL == 0 {
		cfg.ProviderTTL = ProviderTTL
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.MaxRequests < 1 || cfg.MaxPerPeer < 1 || cfg.MaxProviders < 1 || cfg.ProviderTTL < time.Minute || cfg.RequestTimeout <= 0 {
		return nil, wire.ErrLimit
	}
	child, cancel := context.WithCancel(ctx)
	s := &Service{cfg: cfg, table: NewTable(cfg.Host.ID()), ctx: child, cancel: cancel, providers: map[string]providerEntry{}, requests: make(chan struct{}, cfg.MaxRequests), peerRequests: map[wire.PeerID]int{}, dialing: map[wire.PeerID]int{}}
	for _, p := range cfg.Bootstrap {
		if p.ID == cfg.Host.ID() || len(p.Addrs) == 0 {
			continue
		}
		s.table.Add(PeerInfo(p))
	}
	if cfg.PubliclyReachable {
		if err := cfg.Host.Handle(Protocol, cfg.MaxRequests, s.handle); err != nil {
			cancel()
			return nil, err
		}
	}
	return s, nil
}
func (s *Service) Table() *Table { return s.table }

// SetPubliclyReachable toggles server mode from the owner's current AutoNAT v2
// verdict. Private clusters must not construct the service at all.
func (s *Service) SetPubliclyReachable(reachable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if reachable {
		if s.cfg.PubliclyReachable {
			return nil
		}
		if err := s.cfg.Host.Handle(Protocol, s.cfg.MaxRequests, s.handle); err != nil {
			return err
		}
		s.cfg.PubliclyReachable = true
		return nil
	}
	if s.cfg.PubliclyReachable {
		s.cfg.Host.RemoveHandler(Protocol)
		s.cfg.PubliclyReachable = false
	}
	return nil
}

// AddBootstrap resolves configured bootstrap PeerIds through the existing
// dnsaddr Source and retains only literal QUIC-v1 addresses the Nu host dials.
func (s *Service) AddBootstrap(ctx context.Context, src lookup.Source, ids []wire.PeerID) error {
	if src == nil || len(ids) > 32 {
		return wire.ErrLimit
	}
	var errs []error
	for _, id := range ids {
		var addrs [][]byte
		err := src.Lookup(ctx, id, func(a lookup.Address) bool {
			if a.Peer != id || !isQUICAddress(a.Addr) {
				return true
			}
			resolved, e := resolveQUICAddress(ctx, id, a.Addr)
			if e != nil {
				errs = append(errs, e)
				return true
			}
			for _, addr := range resolved {
				if !s.allowRoutingAddress(addr) {
					continue
				}
				addrs = append(addrs, addr.Bytes())
				if len(addrs) >= MaxAddrs {
					return false
				}
			}
			return true
		})
		if err != nil {
			errs = append(errs, err)
		}
		if len(addrs) > 0 {
			s.table.Add(PeerInfo{ID: id, Addrs: addrs})
		}
	}
	return errors.Join(errs...)
}
func isQUICAddress(a wire.Multiaddr) bool {
	text := a.String()
	if !strings.Contains(text, "/udp/") || !strings.Contains(text, "/quic-v1/p2p/") {
		return false
	}
	i := strings.LastIndex(text, "/p2p/")
	if i < 0 {
		return false
	}
	_, err := wire.ParsePeerID(text[i+5:])
	return err == nil
}
func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
	s.cfg.Host.RemoveHandler(Protocol)
	s.wg.Wait()
	return nil
}

const MaxMessagesPerStream = 64

func (s *Service) handle(st *host.Stream) {
	// Amino peers may pipeline multiple delimited RPCs on a single stream. Keep
	// the stream open until clean EOF; Host's dispatcher owns the final Close.
	peer := st.Conn().PeerID()
	select {
	case s.requests <- struct{}{}:
	default:
		st.Reset()
		return
	}
	defer func() { <-s.requests }()
	s.mu.Lock()
	if s.peerRequests[peer] >= s.cfg.MaxPerPeer {
		s.mu.Unlock()
		st.Reset()
		return
	}
	s.peerRequests[peer]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.peerRequests[peer]--
		if s.peerRequests[peer] == 0 {
			delete(s.peerRequests, peer)
		}
		s.mu.Unlock()
	}()
	reader := bufio.NewReader(st)
	for messages := 0; messages < MaxMessagesPerStream; messages++ {
		_ = st.SetDeadline(time.Now().Add(s.cfg.RequestTimeout))
		msg, err := readMessageBuffered(reader)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if s.ctx.Err() == nil && st.Conn().Context().Err() == nil {
				slog.Warn("kad: rejected malformed request", "peer", peer, "err", err)
				st.Reset()
			}
			return
		}
		resp, reply, err := s.handleMessage(peer, msg)
		if err != nil {
			slog.Warn("kad: rejected request", "peer", peer, "type", msg.Type, "err", err)
			st.Reset()
			return
		}
		if reply {
			_ = st.SetDeadline(time.Now().Add(s.cfg.RequestTimeout))
			if err = writeMessage(st, resp); err != nil {
				slog.Warn("kad: response write failed", "peer", peer, "type", msg.Type, "err", err)
				st.Reset()
				return
			}
		}
	}
	st.Reset()
}

func (s *Service) handleMessage(peer wire.PeerID, msg Message) (Message, bool, error) {
	resp := Message{Type: msg.Type, Key: msg.Key, ClusterLevel: msg.ClusterLevel}
	s.learnRequester(peer)
	switch msg.Type {
	case FindNode:
		// FIND_NODE also routes arbitrary provider keys, not only PeerIds.
		if len(msg.Key) == 0 || len(msg.Key) > MaxKey {
			return Message{}, false, wire.ErrLimit
		}
		resp.Closer = s.closestPeerRecords(msg.Key, peer)
	case GetProviders:
		if _, err := cid.DecodeMultihash(msg.Key); err != nil {
			return Message{}, false, err
		}
		resp.Closer = s.closestPeerRecords(msg.Key, peer)
		resp.Providers = s.getProviders(msg.Key)
	case AddProvider:
		if _, err := cid.DecodeMultihash(msg.Key); err != nil {
			return Message{}, false, err
		}
		if len(msg.Providers) != 1 || msg.Providers[0].ID != peer {
			return Message{}, false, ErrMessage
		}
		if !s.validProvider(msg.Providers[0]) || !s.storeProvider(msg.Key, msg.Providers[0]) {
			return Message{}, false, wire.ErrLimit
		}
		return Message{}, false, nil // Amino ADD_PROVIDER is one-way.
	case Ping:
	default:
		return Message{}, false, ErrMessage
	}
	return resp, true, nil
}

// learnRequester refreshes a known routing peer, or adds an inbound peer whose
// identify snapshot says it serves Kademlia at usable addresses. Address-less
// entries are never created: they could not be dialed later.
func (s *Service) learnRequester(peer wire.PeerID) {
	if addrs := s.peerAddresses(peer); len(addrs) > 0 {
		s.table.Add(PeerInfo{ID: peer, Addrs: addrs})
		return
	}
	if s.cfg.ServerAddrs == nil {
		return
	}
	p := Peer{ID: peer}
	for _, a := range s.cfg.ServerAddrs(peer) {
		p.Addrs = append(p.Addrs, a.Bytes())
	}
	if usable, ok := s.usablePeer(p); ok {
		s.table.Add(PeerInfo(usable))
	}
}
func (s *Service) peerAddresses(id wire.PeerID) [][]byte {
	for _, p := range s.table.Peers() {
		if p.ID == id {
			return p.Addrs
		}
	}
	return nil
}
func (s *Service) closestPeerRecords(key []byte, exclude wire.PeerID) []Peer {
	peers := s.table.Closest(key, K+1)
	out := make([]Peer, 0, len(peers))
	for _, p := range peers {
		if p.ID != exclude {
			out = append(out, Peer(p))
		}
		if len(out) == K {
			break
		}
	}
	return out
}
func normalizeProvider(p Peer) (Peer, bool) {
	if _, err := wire.PeerIDFromBytes(p.ID.Bytes()); err != nil || len(p.Addrs) == 0 {
		return Peer{}, false
	}
	out := Peer{ID: p.ID}
	for _, raw := range p.Addrs {
		a, e := wire.MultiaddrFromBytes(raw, 1024)
		if e != nil {
			return Peer{}, false
		}
		normalized, e := lookup.NormalizeAddress(p.ID, a)
		if e != nil {
			return Peer{}, false
		}
		out.Addrs = append(out.Addrs, normalized.Bytes())
	}
	return out, true
}
func (s *Service) validProvider(p Peer) bool { _, ok := normalizeProvider(p); return ok }
func (s *Service) storeProvider(key []byte, p Peer) bool {
	p, ok := normalizeProvider(p)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneProviders(now)
	k := string(key)
	entry, exists := s.providers[k]
	if !exists {
		if len(s.providers) >= s.cfg.MaxProviders {
			return false
		}
		entry = providerEntry{expires: map[wire.PeerID]time.Time{}}
	}
	for i, old := range entry.peers {
		if old.ID == p.ID {
			entry.peers[i] = p
			entry.expires[p.ID] = now.Add(s.cfg.ProviderTTL)
			s.providers[k] = entry
			return true
		}
	}
	if s.providerRecords >= s.cfg.MaxProviders {
		return false
	}
	entry.peers = append(entry.peers, p)
	entry.expires[p.ID] = now.Add(s.cfg.ProviderTTL)
	s.providers[k] = entry
	s.providerRecords++
	return true
}
func (s *Service) pruneProviders(now time.Time) {
	for key, entry := range s.providers {
		live := entry.peers[:0]
		for _, p := range entry.peers {
			if entry.expires[p.ID].After(now) {
				live = append(live, p)
			} else {
				delete(entry.expires, p.ID)
				s.providerRecords--
			}
		}
		entry.peers = live
		if len(live) == 0 {
			delete(s.providers, key)
		} else {
			s.providers[key] = entry
		}
	}
}
func (s *Service) getProviders(key []byte) []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneProviders(time.Now())
	v := s.providers[string(key)].peers
	if len(v) > MaxPeers {
		return append([]Peer(nil), v[len(v)-MaxPeers:]...)
	}
	return append([]Peer(nil), v...)
}

func readMessage(r io.Reader) (Message, error) { return readMessageBuffered(bufio.NewReader(r)) }
func readMessageBuffered(br *bufio.Reader) (Message, error) {
	n, e := varint.Read(br)
	if e != nil {
		return Message{}, e
	}
	if n > MaxMessage {
		return Message{}, wire.ErrLimit
	}
	b := make([]byte, int(n))
	if _, e = io.ReadFull(br, b); e != nil {
		return Message{}, e
	}
	return Decode(b)
}
func writeMessage(w io.Writer, m Message) error {
	b, e := Encode(m)
	if e != nil {
		return e
	}
	head := varint.Append(nil, uint64(len(b)))
	if _, e = w.Write(head); e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}

func (s *Service) usablePeer(p Peer) (Peer, bool) {
	if p.ID == s.cfg.Host.ID() {
		return Peer{}, false
	}
	if _, err := wire.PeerIDFromBytes(p.ID.Bytes()); err != nil {
		return Peer{}, false
	}
	out := Peer{ID: p.ID}
	seen := map[string]bool{}
	for _, raw := range p.Addrs {
		a, err := wire.MultiaddrFromBytes(raw, 1024)
		if err != nil {
			continue
		}
		a, err = lookup.NormalizeAddress(p.ID, a)
		if err != nil || !isQUICAddress(a) {
			continue
		}
		addrs, err := resolveQUICAddress(s.ctx, p.ID, a)
		if err != nil {
			continue
		}
		for _, resolved := range addrs {
			if !s.allowRoutingAddress(resolved) {
				continue
			}
			key := string(resolved.Bytes())
			if !seen[key] && len(out.Addrs) < MaxAddrs {
				out.Addrs = append(out.Addrs, resolved.Bytes())
				seen[key] = true
			}
		}
	}
	return out, len(out.Addrs) > 0
}
func (s *Service) addPeers(ps []Peer) {
	for _, p := range ps {
		if usable, ok := s.usablePeer(p); ok {
			s.table.Add(PeerInfo(usable))
		}
	}
}
func (s *Service) call(ctx context.Context, id wire.PeerID, m Message) (Message, error) {
	if id == s.cfg.Host.ID() {
		return Message{}, errors.New("kad: self RPC")
	}
	if err := s.ensureConnected(ctx, id); err != nil {
		return Message{}, err
	}
	st, err := s.cfg.Host.OpenStream(ctx, id, Protocol)
	if err != nil {
		return Message{}, err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(s.cfg.RequestTimeout))
	if err = writeMessage(st, m); err != nil {
		return Message{}, err
	}
	if m.Type == AddProvider {
		return Message{}, closeRequestStream(st)
	}
	resp, err := readMessage(st)
	if err != nil {
		return Message{}, err
	}
	if err = st.CloseWrite(); err != nil {
		return Message{}, err
	}
	_, err = io.Copy(io.Discard, st)
	if err != nil && !errors.Is(err, io.EOF) {
		return Message{}, err
	}
	return resp, nil
}
func closeRequestStream(st *host.Stream) error {
	if err := st.CloseWrite(); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, st)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
func (s *Service) ensureConnected(ctx context.Context, id wire.PeerID) error {
	if _, ok := s.cfg.Host.Connection(id); ok {
		return nil
	}
	var addrs []wire.Multiaddr
	for _, p := range s.table.Peers() {
		if p.ID == id {
			for _, b := range p.Addrs {
				a, e := wire.MultiaddrFromBytes(b, 1024)
				if e == nil {
					addrs = append(addrs, a)
				}
			}
			break
		}
	}
	if len(addrs) == 0 {
		return host.ErrNoConnection
	}
	if err := s.cfg.Sessions.AddHints(ctx, []lookup.Hint{{Peer: id, Addresses: addrs}}); err != nil {
		return err
	}
	s.mu.Lock()
	s.dialing[id]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.dialing[id]--; s.dialing[id] == 0 {
			delete(s.dialing, id)
		}
		s.mu.Unlock()
	}()
	_, err := s.cfg.Sessions.Connect(ctx, id)
	return err
}
func (s *Service) seedPeers() []PeerInfo {
	p := s.table.Peers()
	seen := make(map[wire.PeerID]bool, len(p))
	for _, peer := range p {
		seen[peer.ID] = true
	}
	for _, c := range s.cfg.Host.Connections() {
		if !seen[c.PeerID()] {
			p = append(p, PeerInfo{ID: c.PeerID()})
		}
	}
	return p
}

// FindNode runs a bounded alpha-parallel iterative lookup. Newly discovered peers
// are learned immediately and each peer is queried at most once.
func (s *Service) FindNode(ctx context.Context, id wire.PeerID) ([]PeerInfo, error) {
	target := id.Bytes()
	return s.iterative(ctx, target, FindNode)
}
func (s *Service) iterative(ctx context.Context, target []byte, op MessageType) ([]PeerInfo, error) {
	if len(target) > MaxKey {
		return nil, wire.ErrLimit
	}
	known := map[wire.PeerID]PeerInfo{}
	for _, p := range s.seedPeers() {
		known[p.ID] = p
	}
	queried := map[wire.PeerID]bool{}
	targetID, targetIsPeer := wire.PeerID(""), false
	if id, err := wire.PeerIDFromBytes(target); err == nil {
		targetID, targetIsPeer = id, true
	}
	var errs []error
	for rounds := 0; rounds < 16; rounds++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates := unqueriedClosest(known, queried, target)
		if len(candidates) == 0 {
			break
		}
		type result struct {
			id wire.PeerID
			m  Message
			e  error
		}
		ch := make(chan result, len(candidates))
		for _, p := range candidates {
			queried[p.ID] = true
			p := p
			go func() { m, e := s.call(ctx, p.ID, Message{Type: op, Key: target}); ch <- result{p.ID, m, e} }()
		}
		for range candidates {
			r := <-ch
			if r.e != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.id, r.e))
				continue
			}
			s.addPeers(r.m.Closer)
			for _, p := range r.m.Closer {
				if usable, ok := s.usablePeer(p); ok {
					known[usable.ID] = PeerInfo(usable)
				}
			}
		}
		if op == FindNode && targetIsPeer {
			if _, found := known[targetID]; found {
				break
			}
		}
	}
	out := make([]PeerInfo, 0, len(known))
	for _, p := range known {
		out = append(out, p)
	}
	sortPeers(out, target)
	if len(out) > K {
		out = out[:K]
	}
	return out, errors.Join(errs...)
}

// unqueriedClosest picks the next Alpha peers to ask: the unqueried ones among the K
// known peers closest to target. When all of those have been asked the lookup has
// converged (standard Kademlia termination) and nothing is returned.
func unqueriedClosest(known map[wire.PeerID]PeerInfo, queried map[wire.PeerID]bool, target []byte) []PeerInfo {
	all := make([]PeerInfo, 0, len(known))
	for _, p := range known {
		all = append(all, p)
	}
	sortPeers(all, target)
	if len(all) > K {
		all = all[:K]
	}
	var out []PeerInfo
	for _, p := range all {
		if !queried[p.ID] {
			out = append(out, p)
			if len(out) == Alpha {
				break
			}
		}
	}
	return out
}
func sortPeers(ps []PeerInfo, target []byte) {
	dst := key(target)
	sort.Slice(ps, func(i, j int) bool { return closer(key(ps[i].ID.Bytes()), key(ps[j].ID.Bytes()), dst) })
}

func (s *Service) FindProviders(ctx context.Context, key []byte) ([]Peer, error) {
	if _, err := cid.DecodeMultihash(key); err != nil {
		return nil, err
	}
	known := make(map[wire.PeerID]PeerInfo)
	for _, p := range s.seedPeers() {
		known[p.ID] = p
	}
	queried := make(map[wire.PeerID]bool)
	providers := make(map[wire.PeerID]Peer)
	for _, p := range s.getProviders(key) {
		if usable, ok := s.usablePeer(p); ok {
			providers[usable.ID] = usable
		}
	}
	var errs []error
	for rounds := 0; rounds < 16; rounds++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(providers) > 0 {
			break // a fetch wants providers now; the K-closest sweep can stop
		}
		candidates := unqueriedClosest(known, queried, key)
		if len(candidates) == 0 {
			break
		}
		type result struct {
			m Message
			e error
		}
		ch := make(chan result, len(candidates))
		for _, p := range candidates {
			queried[p.ID] = true
			p := p
			go func() { m, e := s.call(ctx, p.ID, Message{Type: GetProviders, Key: key}); ch <- result{m, e} }()
		}
		for range candidates {
			r := <-ch
			if r.e != nil {
				errs = append(errs, r.e)
				continue
			}
			s.addPeers(r.m.Closer)
			for _, peer := range r.m.Closer {
				if usable, ok := s.usablePeer(peer); ok {
					known[usable.ID] = PeerInfo(usable)
				}
			}
			for _, p := range r.m.Providers {
				if usable, ok := s.usablePeer(p); ok {
					providers[usable.ID] = usable
				}
			}
		}
	}
	res := make([]Peer, 0, len(providers))
	for _, p := range providers {
		res = append(res, p)
	}
	if len(res) > 0 {
		return res, nil
	}
	return res, errors.Join(errs...)
}
func (s *Service) ProvidePinned(ctx context.Context, cids []cid.CID) error {
	if len(s.cfg.Advertise) == 0 {
		return errors.New("kad: no usable advertised addresses configured")
	}
	if len(cids) > 4096 {
		return wire.ErrLimit
	}
	for _, c := range cids {
		if !c.Defined() {
			return cid.ErrInvalid
		}
		if err := s.ProvideAndRepublish(ctx, c.Multihash().Bytes(), s.cfg.Advertise); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Provide(ctx context.Context, key []byte, addrs []wire.Multiaddr) error {
	if _, err := cid.DecodeMultihash(key); err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("kad: provider requires usable addresses")
	}
	p := Peer{ID: s.cfg.Host.ID()}
	for _, a := range addrs {
		n, e := lookup.NormalizeAddress(p.ID, a)
		if e != nil {
			return e
		}
		p.Addrs = append(p.Addrs, n.Bytes())
	}
	if !s.validProvider(p) {
		return wire.ErrFormat
	}
	s.storeProvider(key, p)
	targets, err := s.iterative(ctx, key, FindNode)
	if len(targets) == 0 {
		return errors.Join(ErrNoPeers, err)
	}
	msg := Message{Type: AddProvider, Key: key, Providers: []Peer{p}}
	var errs []error
	sent := 0
	for _, target := range targets {
		if target.ID == p.ID {
			continue
		}
		if _, e := s.call(ctx, target.ID, msg); e != nil {
			errs = append(errs, fmt.Errorf("%s: %w", target.ID, e))
		} else {
			sent++
		}
	}
	if sent > 0 {
		return nil
	}
	return errors.Join(err, errors.Join(errs...))
}
func (s *Service) ProvideAndRepublish(ctx context.Context, key []byte, addrs []wire.Multiaddr) error {
	if err := s.Provide(ctx, key, addrs); err != nil {
		return err
	}
	copyKey := append([]byte(nil), key...)
	copyAddrs := append([]wire.Multiaddr(nil), addrs...)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(RepublishInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
				_ = s.Provide(ctx, copyKey, copyAddrs)
				cancel()
			}
		}
	}()
	return nil
}
func (s *Service) String() string { return fmt.Sprintf("kad(%s)", s.cfg.Host.ID()) }

// Lookup implements p2p/lookup.Source using iterative FIND_NODE. The session
// manager consults every source while dialing, including dials kad itself makes
// to routing peers: those, and peers already in the table, are answered from the
// table without a network lookup, so a lookup never recurses into itself. An
// empty table (not bootstrapped) answers nothing rather than spraying FIND_NODE
// at connected non-DHT peers.
func (s *Service) Lookup(ctx context.Context, id wire.PeerID, emit func(lookup.Address) bool) error {
	s.mu.Lock()
	dialing := s.dialing[id] > 0
	s.mu.Unlock()
	known := s.peerAddresses(id)
	if dialing || len(known) > 0 {
		s.emitAddresses(id, known, emit)
		return nil
	}
	if len(s.table.Peers()) == 0 {
		return nil
	}
	peers, err := s.FindNode(ctx, id)
	found := false
	for _, p := range peers {
		if p.ID != id {
			continue
		}
		found = true
		if !s.emitAddresses(id, p.Addrs, emit) {
			return nil
		}
	}
	if found {
		return nil
	}
	return err
}
func (s *Service) emitAddresses(id wire.PeerID, addrs [][]byte, emit func(lookup.Address) bool) bool {
	for _, raw := range addrs {
		a, e := wire.MultiaddrFromBytes(raw, 1024)
		if e != nil {
			continue
		}
		if !emit(lookup.Address{Peer: id, Addr: a, Source: "amino-kad", Seen: time.Now(), Expires: time.Now().Add(30 * time.Minute), Confidence: .5}) {
			return false
		}
	}
	return true
}
