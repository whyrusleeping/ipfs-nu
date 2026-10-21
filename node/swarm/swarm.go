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
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	p2pproto "github.com/whyrusleeping/nu/p2p/proto"
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
	// AllowPeer, if set, gates every connection from the moment the host listens.
	AllowPeer host.ConnectionGater
	// Fetch tunes the fetcher (Store is filled in); zero values are the defaults.
	Fetch fetch.Config
	// InterfaceIPs overrides how unspecified listen addresses are expanded (tests).
	InterfaceIPs func() []net.IP
	Logger       *slog.Logger
}

// Swarm is a running network stack. Close it when done; it does not own the store.
type Swarm struct {
	cfg      Config
	log      *slog.Logger
	host     *host.Host
	cache    *lookup.Cache
	lookup   *lookup.Service
	sessions *host.Sessions
	identify *p2pproto.Service
	server   *proto.Server
	fetcher  *fetch.Fetcher
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
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
	s := &Swarm{cfg: cfg, log: log}
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
	s.host, err = host.New(host.Config{Key: cfg.Key, Listen: listen, AllowPeer: cfg.AllowPeer})
	if err != nil && cfg.Ephemeral && errors.Is(err, syscall.EADDRINUSE) {
		log.Info("swarm port in use; using an ephemeral port", "listen", listen)
		s.host, err = host.New(host.Config{Key: cfg.Key, Listen: ephemeral(listen), AllowPeer: cfg.AllowPeer})
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
	lcfg := lookup.Config{Cache: s.cache, DisableMDNS: !cfg.MDNS, MDNS: lookup.MDNSConfig{
		Peer: s.host.ID(), Addresses: s.host.Addrs, Interfaces: cfg.MDNSInterfaces}}
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
	s.sessions, err = host.NewSessions(s.host, host.SessionConfig{Lookup: s.lookup, Cache: s.cache})
	if err != nil {
		return nil, err
	}
	s.identify, err = p2pproto.New(s.host, p2pproto.Config{AgentVersion: nu.Name + "/" + nu.Version})
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
		fc.Providers = connected{s}
		s.fetcher = fetch.New(fc)
	}
	ok = true
	log.Info("swarm started", "peer", s.host.ID().Base36(), "addrs", s.host.Addrs())
	return s, nil
}

// Close stops everything in reverse order. Running fetches fail.
func (s *Swarm) Close() error {
	s.once.Do(func() {
		if s.identify != nil {
			_ = s.identify.Close()
		}
		if s.sessions != nil {
			_ = s.sessions.Close()
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
	return Advertise(s.host.Addrs(), s.cfg.InterfaceIPs)
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
	if s.fetcher == nil {
		return apiv0.Errorf(apiv0.CodeFailed, "this node has no store to fetch into")
	}
	var lh []lookup.Hint
	for _, p := range hints {
		if p.Peer == s.host.ID() {
			continue
		}
		lh = append(lh, lookup.Hint{Peer: p.Peer, Addresses: p.Addrs})
	}
	var peers []fetch.Peer
	hintErr := fetch.Hinted{Sessions: s.sessions, Hints: lh}.Find(ctx, root, func(p fetch.Peer) { peers = append(peers, p) })
	if hintErr != nil {
		s.log.Debug("some link hints are unusable", "err", hintErr)
	}
	if len(peers) == 0 && len(s.host.Connections()) == 0 {
		return apiv0.Errorf(apiv0.CodeUnavailable, "no one to fetch %s from: the link names no reachable provider and no peer is connected", root).
			WithAction("use a share link with its provider (`peer=`/`addr=`), or be on the same network as a node that holds the content")
	}
	opts := fetch.Options{Peers: peers}
	if progress != nil {
		opts.Progress = func(p fetch.Progress) {
			if p.Stage == fetch.StageBlobs || p.Stage == fetch.StageDone {
				progress(p.Bytes, p.BytesTotal)
			}
		}
	}
	_, err := s.fetcher.Fetch(ctx, root, opts)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
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
	switch {
	case errors.Is(err, proto.ErrCorrupt):
		return apiv0.Errorf(apiv0.CodeFailed, "every provider of %s sent corrupt data", root).
			WithAction("try again later or with another provider; corrupt peers are dropped for the rest of a fetch").WithDetail("cause", err.Error())
	case errors.Is(err, proto.ErrNotFound), errors.Is(err, proto.ErrNotHeld):
		return apiv0.Errorf(apiv0.CodeNotFound, "no provider has %s", root).
			WithAction("the holder may have removed it or not finished adding it; ask for a fresh link").WithDetail("cause", err.Error())
	case errors.Is(err, fetch.ErrNoProvider):
		return apiv0.Errorf(apiv0.CodeUnavailable, "no provider of %s could be reached", root).
			WithAction("check that the holder is online (its `"+nu.Name+" daemon` or `"+nu.Name+" share` is running) and reachable over UDP").WithDetail("cause", err.Error())
	}
	return apiv0.Errorf(apiv0.CodeFailed, "fetching %s failed: %v", root, err)
}

// connected offers the connected peers as session-backed providers.
type connected struct{ s *Swarm }

func (c connected) Find(ctx context.Context, _ cid.CID, found func(fetch.Peer)) error {
	for _, id := range c.s.Connected() {
		sess, err := c.s.sessions.Get(id)
		if err != nil {
			continue
		}
		found(fetch.SessionPeer(sess))
	}
	return nil
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
	return Config{Key: key, Store: st, Listen: cfg.Swarm.Listen, MDNS: cfg.Swarm.MDNS, CacheFile: r.Path(repo.PeersFile)}, nil
}
