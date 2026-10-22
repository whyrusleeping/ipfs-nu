// Package swarm assembles a node's networking from the p2p and data-protocol packages:
// the QUIC host with identify/ping, peer lookup (persisted address cache, mDNS), peer
// sessions, the /nu/bytes and /nu/have servers over the store, and a fetcher whose
// providers are share-link hints plus connected peers. A Swarm implements node.Fetcher
// and node.Net; the daemon runs one for its lifetime and daemonless commands run one
// for the command.
package swarm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/portmap"
	p2pproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// Config configures a Swarm. Key is required; without a Store the swarm neither serves
// nor fetches content (a connectivity probe).
type Config struct {
	Key   *wire.NodeKey
	Store *store.Store
	// Listen holds the swarm listen multiaddrs (`/ip4/0.0.0.0/udp/4171/quic-v1`); only
	// literal IP + UDP + quic-v1 entries are bound, one socket per IP family. Empty
	// listens on loopback at an ephemeral port.
	Listen []string
	// Ephemeral retries on ephemeral ports when a configured port is already in use
	// (a one-shot command beside a daemon, or two nodes on one machine).
	Ephemeral bool
	// CacheFile persists the peer-address cache (0600 JSON); empty keeps it in memory.
	CacheFile string
	// MDNS enables LAN discovery. MDNSInterfaces restricts it to given interfaces (an
	// in-process test uses loopback); nil selects the up multicast interfaces.
	MDNS           bool
	MDNSInterfaces []net.Interface
	// MDNSCluster, when set, keeps mDNS on in Private/Isolated mode under the service name
	// derived from this cluster ID (lookup.ServiceName); peers found still pass the gate.
	MDNSCluster string
	// AllowPeer, if set, gates every connection from the moment the host listens.
	AllowPeer host.ConnectionGater
	// Fetch tunes the fetcher (Store is filled in); zero values are the defaults.
	Fetch fetch.Config
	// InterfaceIPs overrides how unspecified listen addresses are expanded (tests).
	InterfaceIPs func() []net.IP
	Logger       *slog.Logger
	// Public connectivity uses only explicit configured/known relays. Private and
	// Isolated disable relay/AutoNAT/disco/default public discovery entirely.
	Relays            []wire.Multiaddr
	WSS               WSSPolicy
	Private, Isolated bool
	Peers             map[wire.PeerID][]wire.Multiaddr
	// LookupSources is the public streaming resolver attachment point (0072).
	// Sources must honor cancellation; the caller owns their service lifetimes.
	LookupSources []lookup.Source
	// ProbeAllowed is an independent exact-endpoint policy, never populated from
	// peer records. Grants also permit private candidate disclosure to that peer.
	ProbeAllowed map[wire.PeerID][]netip.AddrPort
	// PortMapping opts in to leases at an explicitly selected gateway.
	PortMapping *portmap.Config
	// DHT configures Amino participation and the public lookup sources (dht.go).
	DHT DHTConfig
	// NAT overrides the profile for status/doctor. Nil uses the built-in netcheck
	// monitor, falling back to identify observations until a report is available.
	NAT NATProfiler
	// PeerRelayAllow turns this public node into a bounded circuit relay (with the
	// datagram extension) for exactly these peers: an owner's devices or an
	// allow-list. Empty disables peer relaying; private/isolated modes never relay.
	PeerRelayAllow []wire.PeerID
}

// Swarm is a running network stack. Close it when done; it does not own the store.
type Swarm struct {
	cfg          Config
	log          *slog.Logger
	host         *host.Host
	cache        *lookup.Cache
	lookup       *lookup.Service
	sessions     *host.Sessions
	identify     *p2pproto.Service
	server       *proto.Server
	fetcher      *fetch.Fetcher
	cancel       context.CancelFunc
	once         sync.Once
	closeErr     error
	connectivity *connectivity
	dht          *dht
	metrics      *netMetrics
	peerRelay    *circuit.Server // relay_optimizer.go; nil unless PeerRelayAllow
}

// Start binds the host and starts the services. The returned Swarm is independent of
// ctx once started; ctx bounds Start itself.
func Start(ctx context.Context, cfg Config) (*Swarm, error) {
	if cfg.Key == nil {
		return nil, errors.New("swarm: Key is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.InterfaceIPs == nil {
		cfg.InterfaceIPs = InterfaceIPs
	}
	if cfg.Private {
		gate := cfg.AllowPeer
		members := make(map[wire.PeerID]bool, len(cfg.Peers))
		for id := range cfg.Peers {
			members[id] = true
		}
		cfg.AllowPeer = func(id wire.PeerID) bool { return members[id] && (gate == nil || gate(id)) }
	}
	s := &Swarm{cfg: cfg, log: log, metrics: newNetMetrics()}
	if s.cfg.NAT == nil {
		s.cfg.NAT = netcheckNATProfiler{s}
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()

	listen, err := listenAddrs(cfg.Listen, log)
	if err != nil {
		return nil, err
	}
	var socket *magicsock.Conn
	s.host, socket, err = sharedHost(host.Config{Key: cfg.Key, Listen: listen, AllowPeer: cfg.AllowPeer})
	if err != nil && cfg.Ephemeral && errors.Is(err, syscall.EADDRINUSE) {
		log.Info("swarm port in use; using an ephemeral port", "listen", listen)
		s.host, socket, err = sharedHost(host.Config{Key: cfg.Key, Listen: ephemeral(listen), AllowPeer: cfg.AllowPeer})
	}
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, apiv0.Errorf(apiv0.CodeUnavailable, "the swarm port is already in use (%v)", err).
				WithAction("stop the other program or daemon using it, or change it with `" + nu.Name + " config set swarm.listen '[\"/ip4/0.0.0.0/udp/<port>/quic-v1\"]'`")
		}
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "cannot listen for peers: %v", err).
			WithAction("check swarm.listen in the config and that UDP sockets can be opened")
	}

	s.cache, err = lookup.NewCache(bg, lookup.CacheConfig{Persistence: persistence(cfg.CacheFile)})
	if err != nil && cfg.CacheFile != "" {
		log.Warn("peer cache unusable; starting without it", "file", cfg.CacheFile, "err", err)
		s.cache, err = lookup.NewCache(bg, lookup.CacheConfig{})
	}
	if err != nil {
		return nil, err
	}
	sources := append([]lookup.Source(nil), cfg.LookupSources...)
	if cfg.Private || cfg.Isolated {
		sources = nil
	} else {
		public, err := s.publicSources(cfg)
		if err != nil {
			return nil, err
		}
		sources = append(append(sources, lookup.SourceFunc(s.signedAddresses)), public...)
	}
	lcfg := lookup.Config{Cache: s.cache, Sources: sources, DisableMDNS: !cfg.MDNS || (cfg.Private || cfg.Isolated) && cfg.MDNSCluster == "", MDNS: lookup.MDNSConfig{
		Peer: s.host.ID(), Addresses: s.host.Addrs, Interfaces: cfg.MDNSInterfaces, ClusterID: cfg.MDNSCluster}}
	s.lookup, err = lookup.New(bg, lcfg)
	if err != nil && cfg.MDNS {
		log.Warn("LAN discovery (mDNS) unavailable; peers need addresses in links", "err", err)
		lcfg.DisableMDNS = true
		s.lookup, err = lookup.New(bg, lcfg)
	}
	if err != nil {
		return nil, err
	}
	// Sessions before application handlers, so their gating is in place when peers arrive.
	if err := s.startConnectivity(bg, socket); err != nil {
		return nil, err
	}
	if err := s.startPeerRelay(cfg.Key); err != nil {
		return nil, err
	}
	// One route may need independent UDP, WSS and inner-QUIC setup budgets.
	s.sessions, err = host.NewSessions(s.host, host.SessionConfig{Lookup: s.lookup, Cache: s.cache, Dial: s.connectivity.dial, DialTimeout: 6 * time.Second, ConnectTimeout: 8 * time.Second, Private: cfg.Private, Peers: cfg.Peers})
	if err != nil {
		return nil, err
	}
	if cfg.Store != nil {
		s.server = proto.NewServer(cfg.Store, proto.ServerConfig{})
		if err := s.server.Register(s.host); err != nil {
			return nil, err
		}
		fc := cfg.Fetch
		fc.Store = cfg.Store
		fc.Providers = fetch.Multi{connected{s}, dhtProviders{s}}
		s.fetcher = fetch.New(fc)
	}
	if err := s.startDHT(bg); err != nil {
		return nil, err
	}
	ok = true
	log.Info("swarm started", "peer", s.host.ID().Base36(), "addrs", s.host.Addrs())
	return s, nil
}

// Close stops everything in reverse order. Running fetches fail.
func (s *Swarm) Close() error {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.dht != nil {
			s.dht.close()
		}
		if s.sessions != nil {
			_ = s.sessions.Close()
		}
		if s.connectivity != nil {
			s.connectivity.close()
		}
		if s.peerRelay != nil {
			_ = s.peerRelay.Close()
		}
		if s.identify != nil {
			_ = s.identify.Close()
		}
		if s.lookup != nil {
			_ = s.lookup.Close()
		}
		if s.host != nil {
			s.closeErr = s.host.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	return s.closeErr
}

// Host, Sessions, Lookup and Server expose the components (harnesses, diagnostics).
func (s *Swarm) Host() *host.Host         { return s.host }
func (s *Swarm) Sessions() *host.Sessions { return s.sessions }
func (s *Swarm) Lookup() *lookup.Service  { return s.lookup }
func (s *Swarm) Server() *proto.Server    { return s.server }

// ID is the host's PeerId.
func (s *Swarm) ID() wire.PeerID { return s.host.ID() }

// ListenAddrs are the bound addresses as the host reports them (unspecified IPs as such).
func (s *Swarm) ListenAddrs() []wire.Multiaddr { return s.host.Addrs() }

// Addrs are the addresses to advertise (node.Net): bound addresses with unspecified IPs
// expanded over the machine's usable interface addresses of the same family; loopback
// only when nothing else is bound, so an in-process test host advertises 127.0.0.1.
func (s *Swarm) Addrs() []wire.Multiaddr {
	addrs := Advertise(s.host.Addrs(), s.cfg.InterfaceIPs)
	if s.connectivity != nil {
		s.connectivity.mu.Lock()
		homes := s.connectivity.homes
		network, closed := s.connectivity.network, s.connectivity.closed
		s.connectivity.mu.Unlock()
		if network != nil && !closed {
			network.mu.Lock()
			confirmed := append([]wire.Multiaddr(nil), network.confirmed...)
			network.mu.Unlock()
			addrs = append(confirmed, addrs...)
		}
		if homes != nil {
			// Links cap hints at three: keep BOTH relay routes ahead of direct
			// expansion so a multi-interface node cannot truncate its backup.
			addrs = append(homes.Addresses(), addrs...)
		}
	}
	seen := make(map[string]bool, len(addrs))
	unique := make([]wire.Multiaddr, 0, len(addrs))
	for _, addr := range addrs {
		if s.host.IsLocalOnlyMultiaddr(addr) {
			continue
		}
		if !seen[addr.String()] {
			seen[addr.String()] = true
			unique = append(unique, addr)
		}
	}
	return unique
}

// Peers counts connected peers (node.Net).
func (s *Swarm) Peers() int { return len(s.Connected()) }

// Connected lists the connected peers.
func (s *Swarm) Connected() []wire.PeerID {
	seen := map[wire.PeerID]bool{}
	var out []wire.PeerID
	for _, c := range s.host.Connections() {
		if id := c.PeerID(); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Connect dials a peer at the given addresses (without /p2p suffix or with a matching
// one) and waits for the authenticated connection. Addresses are remembered as hints.
func (s *Swarm) Connect(ctx context.Context, id wire.PeerID, addrs []wire.Multiaddr) error {
	if err := s.connectivity.admitHints(id, addrs); err != nil {
		return err
	}
	if err := s.sessions.AddHints(ctx, []lookup.Hint{{Peer: id, Addresses: addrs}}); err != nil {
		return err
	}
	_, err := s.sessions.Connect(ctx, id)
	return err
}

// Ping measures the round trip to a connected (or dialable) peer.
func (s *Swarm) Ping(ctx context.Context, id wire.PeerID) (time.Duration, error) {
	if _, err := s.sessions.Connect(ctx, id); err != nil {
		return 0, err
	}
	return s.identify.Ping(ctx, id)
}

// Fetch implements node.Fetcher: hinted providers are tried together with connected
// peers; everything is verified before it is stored.
func (s *Swarm) Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error {
	return s.FetchWithSource(ctx, root, hints, nil, progress)
}

// FetchWithSource implements node.SourceFetcher: src (RASL hosts), when non-nil, is
// asked for every object alongside the peers, and suffices on its own.
func (s *Swarm) FetchWithSource(ctx context.Context, root cid.CID, hints []link.Provider, src fetch.Source, progress func(done, total int64)) error {
	if s.fetcher == nil {
		return apiv0.Errorf(apiv0.CodeFailed, "this node has no store to fetch into")
	}
	var lh []lookup.Hint
	for _, p := range hints {
		if p.Peer == s.host.ID() {
			continue
		}
		if err := s.connectivity.admitHints(p.Peer, p.Addrs); err != nil {
			return err
		}
		lh = append(lh, lookup.Hint{Peer: p.Peer, Addresses: p.Addrs})
	}
	var peers []fetch.Peer
	hintErr := fetch.Hinted{Sessions: s.sessions, Hints: lh}.Find(ctx, root, func(p fetch.Peer) { peers = append(peers, p) })
	if hintErr != nil {
		s.log.Debug("some link hints are unusable", "err", hintErr)
	}
	if len(peers) == 0 && len(s.host.Connections()) == 0 && src == nil && s.DHT() == nil {
		return apiv0.Errorf(apiv0.CodeUnavailable, "no one to fetch %s from: the link names no reachable provider and no peer is connected", root).
			WithAction("use a share link with its provider (`peer=`/`addr=`), be on the same network as a node that holds the content, or bind a non-loopback swarm.listen so the DHT can be used")
	}
	opts := fetch.Options{Peers: peers}
	if src != nil {
		opts.Source = src
	}
	observed := s.fetchObserved(time.Now(), progress)
	opts.Progress = func(p fetch.Progress) {
		if p.Stage == fetch.StageBlobs || p.Stage == fetch.StageDone {
			observed(p.Bytes, p.BytesTotal)
		}
	}
	_, err := s.fetcher.Fetch(ctx, root, opts)
	s.metrics.fetches.Add(1)
	if err == nil {
		return nil
	}
	s.metrics.fetchFailures.Add(1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if src != nil {
		if rerr := node.ClassifyRASL(root, err); rerr != err {
			var ae *apiv0.Error
			if errors.As(rerr, &ae) && ae.Code != apiv0.CodeUnavailable {
				return rerr
			}
		}
	}
	return classify(root, err)
}

// FetchCatalog fetches a collection catalog (header, container directories and item
// metadata, not item contents) from connected peers.
func (s *Swarm) FetchCatalog(ctx context.Context, root cid.CID) error {
	if s.fetcher == nil {
		return apiv0.Errorf(apiv0.CodeFailed, "this node has no store to fetch into")
	}
	_, err := s.fetcher.Fetch(ctx, root, fetch.Options{Catalog: true})
	if err == nil || ctx.Err() != nil {
		return err
	}
	return classify(root, err)
}

// classify turns a fetch failure into the API error contract: not_found when a provider
// answered that it does not hold the content, unavailable when none could be reached.
func classify(root cid.CID, err error) error {
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return ae
	}
	var be *store.BudgetError
	if errors.As(err, &be) {
		return content.InsufficientStorage(be)
	}
	switch {
	case errors.Is(err, proto.ErrCorrupt):
		return apiv0.Errorf(apiv0.CodeFailed, "every provider of %s sent corrupt data", root).
			WithAction("try again later or with another provider; corrupt peers are dropped for the rest of a fetch").WithDetail("cause", err.Error())
	case errors.Is(err, proto.ErrNotFound), errors.Is(err, proto.ErrNotHeld):
		return apiv0.Errorf(apiv0.CodeNotFound, "no provider has %s", root).
			WithAction("the holder may have removed it or not finished adding it; ask for a fresh link").WithDetail("cause", err.Error())
	case errors.Is(err, fetch.ErrNoProvider):
		return apiv0.Errorf(apiv0.CodeUnavailable, "no provider of %s could be reached", root).
			WithAction("check that the holder is online (its `"+nu.Name+" daemon` or `"+nu.Name+" share` is running) and reachable over UDP or a declared WSS relay").WithDetail("cause", err.Error())
	case errors.Is(err, kad.ErrNoPeers):
		return apiv0.Errorf(apiv0.CodeUnavailable, "no provider of %s is known: no link hint, no connected peer, and the DHT has no routing peers", root).
			WithAction("check internet access (DHT bootstrap needs DNS and UDP), or use a share link with its provider (`peer=`/`addr=`)").WithDetail("cause", err.Error())
	}
	return apiv0.Errorf(apiv0.CodeFailed, "fetching %s failed: %v", root, err)
}

// connected offers the connected peers as session-backed providers (D15 step 4), except
// peers whose identify snapshot shows they serve no Nu data protocol (Amino routing
// peers, relays): asking them would only burn request timeouts.
type connected struct{ s *Swarm }

func (c connected) Find(ctx context.Context, _ cid.CID, found func(fetch.Peer)) error {
	for _, id := range c.s.Connected() {
		if !c.s.servesNuData(ctx, id) {
			continue
		}
		sess, err := c.s.sessions.Get(id)
		if err != nil {
			continue
		}
		found(fetch.SessionPeer(sess))
	}
	return nil
}

// servesNuData is true for peers whose identify snapshot advertises a /nu data
// protocol. A peer connected a moment ago (a LAN neighbour) gets a short wait for its
// identify; peers that never identify, or identify without Nu protocols, are skipped.
func (s *Swarm) servesNuData(ctx context.Context, id wire.PeerID) bool {
	if s.identify == nil {
		return true
	}
	ready := func(info p2pproto.PeerInfo) bool { return len(info.Message.Protocols) > 0 }
	info, ok := s.identify.Info(id)
	if !ok || !ready(info) {
		wait, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		var err error
		if info, err = s.identify.WaitPeer(wait, id, ready); err != nil {
			return false
		}
	}
	for _, p := range info.Message.Protocols {
		if p == proto.BytesID || p == proto.HaveID || p == proto.TreeID {
			return true
		}
	}
	return false
}

func persistence(file string) lookup.Persistence {
	if file == "" {
		return nil
	}
	return lookup.FileStore{Path: file}
}

// listenAddrs converts swarm listen multiaddrs to host listen addresses: one literal
// IP/UDP/quic-v1 entry per family; others are skipped with a warning.
func listenAddrs(listen []string, log *slog.Logger) ([]string, error) {
	var out []string
	seen := map[bool]bool{} // v4?
	for _, s := range listen {
		parts := strings.Split(strings.TrimPrefix(s, "/"), "/")
		if len(parts) != 5 || (parts[0] != "ip4" && parts[0] != "ip6") || parts[2] != "udp" || parts[4] != "quic-v1" {
			log.Warn("swarm.listen entry is not /ip4|ip6/<ip>/udp/<port>/quic-v1; skipped", "addr", s)
			continue
		}
		ip := net.ParseIP(parts[1])
		port, err := strconv.Atoi(parts[3])
		if ip == nil || err != nil || port < 0 || port > 65535 {
			log.Warn("swarm.listen entry is malformed; skipped", "addr", s)
			continue
		}
		v4 := ip.To4() != nil
		if (parts[0] == "ip4") != v4 || seen[v4] {
			continue
		}
		seen[v4] = true
		out = append(out, net.JoinHostPort(ip.String(), parts[3]))
	}
	if len(out) == 0 && len(listen) > 0 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "swarm.listen has no usable address").
			WithAction("use entries like /ip4/0.0.0.0/udp/4171/quic-v1")
	}
	return out, nil
}

func ephemeral(listen []string) []string {
	out := make([]string, 0, len(listen))
	for _, l := range listen {
		h, _, err := net.SplitHostPort(l)
		if err != nil {
			continue
		}
		out = append(out, net.JoinHostPort(h, "0"))
	}
	return out
}

// Advertise expands bound addresses for links: an unspecified IP becomes one address per
// usable interface IP of its family (or the family's loopback when there is none), and
// concrete addresses are kept.
func Advertise(bound []wire.Multiaddr, ips func() []net.IP) []wire.Multiaddr {
	var local []net.IP
	var out []wire.Multiaddr
	add := func(text string) {
		if a, err := wire.ParseMultiaddr(text, 1024); err == nil {
			out = append(out, a)
		}
	}
	for _, a := range bound {
		text := a.String()
		parts := strings.Split(text, "/")
		if len(parts) < 3 {
			continue
		}
		ip := net.ParseIP(parts[2])
		if ip == nil || !ip.IsUnspecified() {
			add(text)
			continue
		}
		if local == nil {
			local = ips()
		}
		v4 := parts[1] == "ip4"
		n := 0
		for _, lip := range local {
			if (lip.To4() != nil) != v4 {
				continue
			}
			parts[2] = lip.String()
			add(strings.Join(parts, "/"))
			n++
		}
		if n == 0 {
			parts[2] = "127.0.0.1"
			if !v4 {
				parts[2] = "::1"
			}
			add(strings.Join(parts, "/"))
		}
	}
	return out
}

// InterfaceIPs lists the usable addresses of the up, non-loopback interfaces: all IPv4,
// and IPv6 global unicast (no link-local, no loopback).
func InterfaceIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsUnspecified() {
				continue
			}
			if ipn.IP.To4() == nil && !ipn.IP.IsGlobalUnicast() {
				continue
			}
			ips = append(ips, ipn.IP)
		}
	}
	return ips
}

// LoopbackInterfaces returns the loopback interfaces, for mDNS in in-process tests.
func LoopbackInterfaces() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.Interface
	for _, i := range all {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no loopback interface: %w", os.ErrNotExist)
	}
	return out, nil
}

// RepoConfig is the Config a repo's config.toml describes: swarm.listen, swarm.mdns and
// the peer cache under the repo root.
func RepoConfig(r *repo.Repo, key *wire.NodeKey, st *store.Store) (Config, error) {
	cfg, err := r.Config()
	if err != nil {
		return Config{}, err
	}
	out := Config{Key: key, Store: st, Listen: cfg.Swarm.Listen, MDNS: cfg.Swarm.MDNS, CacheFile: r.Path(repo.PeersFile),
		DHT: DHTConfig{Mode: cfg.Swarm.DHT, LookupDomain: cfg.Swarm.LookupDomain, RoutingURL: cfg.Swarm.RoutingURL, NoAnnounce: r.NoAnnounced}}
	if err := applyConnectivityConfig(&out, cfg.Swarm); err != nil {
		return Config{}, err
	}
	return out, nil
}

// FetchPlaced fetches root for cluster placement (docs/cluster.md "Sync, handoff and
// repair"): all of its structure, and the blobs want selects, from the connected members
// among members, each object tried first at the peers rank names (owners by rank, then
// other members). There is no DHT lookup.
func (s *Swarm) FetchPlaced(ctx context.Context, root cid.CID, members []wire.PeerID, want func(cid.CID) bool, rank func(cid.CID) []wire.PeerID, have []cid.CID) error {
	return s.FetchPlacedProfile(ctx, root, members, want, rank, have, "nu-tree")
}

// FetchPlacedProfile also supports atrepo dataset structure and blob policy.
func (s *Swarm) FetchPlacedProfile(ctx context.Context, root cid.CID, members []wire.PeerID, want func(cid.CID) bool, rank func(cid.CID) []wire.PeerID, have []cid.CID, profile string) error {
	if s.fetcher == nil {
		return apiv0.Errorf(apiv0.CodeFailed, "this node has no store to fetch into")
	}
	var peers []fetch.Peer
	for _, id := range members {
		if id == s.host.ID() {
			continue
		}
		if sess, err := s.sessions.Get(id); err == nil {
			peers = append(peers, fetch.SessionPeer(sess))
		}
	}
	if len(peers) == 0 {
		return apiv0.Errorf(apiv0.CodeUnavailable, "no cluster member is connected to fetch %s from", root)
	}
	_, err := s.fetcher.Fetch(ctx, root, fetch.Options{Peers: peers, Want: want, Rank: rank, Have: have, Profile: profile})
	if err == nil || ctx.Err() != nil {
		return err
	}
	return classify(root, err)
}
