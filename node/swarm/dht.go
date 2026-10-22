package swarm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// DHTConfig configures Amino DHT participation (0072). Only public-mode swarms run it;
// Private/Isolated ignore it entirely.
type DHTConfig struct {
	// Mode is swarm.dht: "" or auto (run when a non-loopback address is bound: a
	// loopback-only socket cannot reach Amino), client (always run, as a client until
	// AutoNAT confirms public reachability), off.
	Mode string
	// Bootstrap replaces the deployed Amino bootstrap peers (in-process tests, labs).
	Bootstrap []kad.Peer
	// AllowPrivateAddresses accepts loopback/LAN routing and provider addresses and
	// announces the swarm's LAN addresses (in-process tests, labs only).
	AllowPrivateAddresses bool
	// Server runs server mode without an AutoNAT verdict (in-process tests, labs only).
	Server bool
	// LookupDomain (dnsaddr peer domain) and RoutingURL (routing/v1 base) add the
	// public peer-address sources of D13; empty leaves them off.
	LookupDomain, RoutingURL string
	// NoAnnounce lists roots (CID strings) never to provide (repo.NoAnnounced).
	NoAnnounce func() ([]string, error)
}

// MaxProvideKeys bounds the keys one provide cycle announces.
const MaxProvideKeys = 4096

// ProvideRefresh is how often the pinned set is re-announced (below the 48 h record TTL).
var ProvideRefresh = kad.RepublishInterval

// dht owns the kad.Service lifecycle: bootstrap, periodic refresh, server-mode
// switching from the connectivity owner's AutoNAT verdict, and D15 providing.
type dht struct {
	s        *Swarm
	svc      *kad.Service
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	ready    chan struct{} // closed after the first bootstrap attempt
	announce chan cid.CID

	mu           sync.Mutex
	bootstrapped time.Time
	bootstrapErr string
	lastProvide  time.Time
	provideErr   string
	provided     int
	announcing   []wire.Multiaddr
	closed       bool
}

// dhtEnabled decides from the config whether this swarm runs Amino at all.
func dhtEnabled(cfg Config) (bool, error) {
	if cfg.Private || cfg.Isolated {
		return false, nil
	}
	switch cfg.DHT.Mode {
	case "off":
		return false, nil
	case "client":
		return true, nil
	case "", "auto":
	default:
		return false, fmt.Errorf("swarm.dht: expected auto, client or off, got %q", cfg.DHT.Mode)
	}
	for _, l := range cfg.Listen {
		parts := strings.Split(strings.TrimPrefix(l, "/"), "/")
		if len(parts) < 2 {
			continue
		}
		if ip := net.ParseIP(parts[1]); ip != nil && !ip.IsLoopback() {
			return true, nil
		}
	}
	return false, nil
}

// publicSources are the D13 sources a public swarm adds behind the lookup service: the
// DHT (lazily, since the service starts after sessions exist), a configured dnsaddr
// peer domain and a configured routing/v1 endpoint.
func (s *Swarm) publicSources(cfg Config) ([]lookup.Source, error) {
	sources := []lookup.Source{lookup.SourceFunc(func(ctx context.Context, id wire.PeerID, emit func(lookup.Address) bool) error {
		if d := s.DHT(); d != nil {
			return d.Lookup(ctx, id, emit)
		}
		return nil
	})}
	if cfg.DHT.LookupDomain != "" {
		dns, err := lookup.NewDNS(lookup.DNSConfig{Domain: cfg.DHT.LookupDomain})
		if err != nil {
			return nil, fmt.Errorf("swarm.lookup_domain: %w", err)
		}
		sources = append(sources, dns)
	}
	if cfg.DHT.RoutingURL != "" {
		src, err := lookup.NewHTTPSource(httpsrc.New(httpsrc.Config{}), cfg.DHT.RoutingURL)
		if err != nil {
			return nil, fmt.Errorf("swarm.routing_url: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, nil
}

func (s *Swarm) startDHT(ctx context.Context) error {
	on, err := dhtEnabled(s.cfg)
	if err != nil || !on {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &dht{s: s, ctx: ctx, cancel: cancel, ready: make(chan struct{}), announce: make(chan cid.CID, 64)}
	d.svc, err = kad.New(ctx, kad.Config{Host: s.host, Sessions: s.sessions, PubliclyReachable: s.cfg.DHT.Server,
		Bootstrap: s.cfg.DHT.Bootstrap, AllowPrivateAddresses: s.cfg.DHT.AllowPrivateAddresses, ServerAddrs: s.kadServerAddrs})
	if err != nil {
		cancel()
		return err
	}
	s.dht = d
	d.wg.Add(1)
	go func() { defer d.wg.Done(); d.run() }()
	return nil
}

// DHT returns the running Amino service, or nil (private/isolated, off, loopback-only,
// or closed by DisablePublic).
func (s *Swarm) DHT() *kad.Service {
	if d := s.dht; d != nil && d.active() {
		return d.svc
	}
	return nil
}
func (d *dht) active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closed
}

// kadServerAddrs feeds the kad routing table from inbound requests: only peers whose
// identify snapshot advertises /ipfs/kad/1.0.0 (servers) at listen addresses.
func (s *Swarm) kadServerAddrs(id wire.PeerID) []wire.Multiaddr {
	if s.identify == nil {
		return nil
	}
	info, ok := s.identify.Info(id)
	if !ok {
		return nil
	}
	for _, p := range info.Message.Protocols {
		if p == kad.Protocol {
			return info.Message.ListenAddrs
		}
	}
	return nil
}

// dhtReachable is the connectivity owner's AutoNAT hook: server mode follows the
// two-observer verdict; anything else drops back to client mode.
func (s *Swarm) dhtReachable(public bool) {
	svc := s.DHT()
	if svc == nil || s.cfg.DHT.Server {
		return
	}
	if err := svc.SetPubliclyReachable(public); err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Warn("DHT server mode change failed", "public", public, "err", err)
	}
}

func (d *dht) close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	d.cancel()
	_ = d.svc.Close()
	d.wg.Wait()
}

// run bootstraps, then keeps the table fresh and the pinned set announced.
func (d *dht) run() {
	d.bootstrap()
	close(d.ready)
	d.provideAll()
	refresh := time.NewTicker(5 * time.Minute)
	defer refresh.Stop()
	republish := time.NewTicker(ProvideRefresh)
	defer republish.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-refresh.C:
			d.refresh()
		case <-republish.C:
			d.provideAll()
		case root := <-d.announce:
			d.provideRoot(root)
		}
	}
}

func (d *dht) bootstrap() {
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	var err error
	if len(d.s.cfg.DHT.Bootstrap) == 0 {
		var dns *lookup.DNS
		dns, err = lookup.NewDNS(lookup.DNSConfig{})
		if err == nil {
			err = d.svc.BootstrapDNS(ctx, dns)
		}
	}
	if len(d.svc.Table().Peers()) > 0 {
		// A self lookup fills the nearest buckets; its per-peer errors are not fatal.
		if _, e := d.svc.FindNode(ctx, d.s.host.ID()); e != nil && err == nil && len(d.svc.Table().Peers()) == 0 {
			err = e
		}
	} else if err == nil {
		err = kad.ErrNoPeers
	}
	d.mu.Lock()
	d.bootstrapped = time.Now()
	d.bootstrapErr = ""
	if err != nil {
		d.bootstrapErr = err.Error()
	}
	d.mu.Unlock()
	if err != nil {
		d.s.log.Warn("DHT bootstrap incomplete", "err", err, "routing_peers", len(d.svc.Table().Peers()))
	} else {
		d.s.log.Info("DHT bootstrapped", "routing_peers", len(d.svc.Table().Peers()))
	}
}

func (d *dht) refresh() {
	if len(d.svc.Table().Peers()) < kad.K {
		d.bootstrap()
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	_, _ = d.svc.FindNode(ctx, d.s.host.ID())
}

// Ready blocks until the first bootstrap attempt has finished (or ctx ends).
func (d *dht) Ready(ctx context.Context) error {
	select {
	case <-d.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// provideAddrs are the addresses put into our provider records: the public
// publication set (confirmed/interface public addresses and known relay circuits);
// LAN addresses only under the explicit private-address override.
func (d *dht) provideAddrs() []wire.Multiaddr {
	if d.s.cfg.DHT.AllowPrivateAddresses {
		return d.s.Addrs()
	}
	return d.s.publicationAddrs()
}

// Announce schedules providing for a freshly pinned root (node pin/add hook). It never
// blocks; a full queue is caught up by the periodic cycle.
func (s *Swarm) Announce(root cid.CID) {
	if s.DHT() == nil || !root.Defined() {
		return
	}
	select {
	case s.dht.announce <- root:
	default:
		s.log.Debug("DHT announce queue full; the periodic cycle will provide it", "cid", root)
	}
}

func (d *dht) provideRoot(root cid.CID) {
	pins, err := d.s.cfg.Store.Pins()
	if err != nil {
		return
	}
	var mine []store.Pin
	for _, p := range pins {
		if p.CID == root {
			mine = append(mine, p)
		}
	}
	keys, err := d.eligible(mine)
	if err != nil || len(keys) == 0 {
		return
	}
	d.provide(keys)
}

func (d *dht) provideAll() {
	if d.s.cfg.Store == nil {
		return
	}
	pins, err := d.s.cfg.Store.Pins()
	if err == nil {
		var keys []cid.CID
		keys, err = d.eligible(pins)
		if err == nil {
			d.provide(keys)
			return
		}
	}
	d.mu.Lock()
	d.lastProvide, d.provideErr = time.Now(), err.Error()
	d.mu.Unlock()
}

// provide announces keys with the current addresses, sequentially and bounded.
func (d *dht) provide(keys []cid.CID) {
	addrs := d.provideAddrs()
	d.mu.Lock()
	d.announcing = addrs
	d.mu.Unlock()
	if len(addrs) == 0 {
		d.mu.Lock()
		d.lastProvide, d.provided, d.provideErr = time.Now(), 0, "no public address to announce (not confirmed reachable and no relay)"
		d.mu.Unlock()
		return
	}
	var errs []error
	n := 0
	for _, c := range keys {
		if d.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(d.ctx, time.Minute)
		err := d.svc.Provide(ctx, c.Multihash().Bytes(), addrs)
		cancel()
		if err != nil {
			if len(errs) < 4 {
				errs = append(errs, fmt.Errorf("%s: %w", c, err))
			}
			continue
		}
		n++
	}
	d.mu.Lock()
	d.lastProvide, d.provided, d.provideErr = time.Now(), n, ""
	if len(errs) > 0 {
		d.provideErr = errors.Join(errs...).Error()
	}
	d.mu.Unlock()
	d.s.log.Info("DHT provide cycle", "keys", len(keys), "provided", n, "errors", len(errs))
}

// announceSources are the pin sources with public intent (D15; cluster.md "network
// modes"): explicit adds/pins, `nu public add`, the node's own names and follows.
// Cluster, surplus, collection and push pins are member-only dataset content and are
// never provided; so are IPFS imports, whose roots become dataset heads.
var announceSources = map[string]bool{store.SourceExplicit: true, store.SourcePublic: true, store.SourceMyNames: true, store.SourceFollow: true}

// announceable reports whether a pin's content may appear in provider records.
func announceable(p store.Pin) bool {
	if !announceSources[p.Source] {
		return false
	}
	return !strings.HasPrefix(p.Name, "ipfs-import:") && !strings.HasPrefix(p.Name, "ipfs-manifest:")
}

// eligible enumerates the D15 announce set of the given pins: each announceable pin's
// root, and for recursive nu-tree pins every file node and blob beneath it; never
// directory shards or headers' interior nodes. Follow pins announce their root only
// (the swarm key is not implemented yet). Roots marked --no-announce are skipped.
func (d *dht) eligible(pins []store.Pin) ([]cid.CID, error) {
	skip := map[string]bool{}
	if d.s.cfg.DHT.NoAnnounce != nil {
		list, err := d.s.cfg.DHT.NoAnnounce()
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			skip[c] = true
		}
	}
	st := d.s.cfg.Store
	seen := map[cid.Multihash]bool{}
	var out []cid.CID
	add := func(c cid.CID) bool {
		if seen[c.Multihash()] {
			return true
		}
		seen[c.Multihash()] = true
		out = append(out, c)
		return len(out) < MaxProvideKeys
	}
	errFull := errors.New("full")
	for _, p := range pins {
		if skip[p.CID.String()] || !announceable(p) {
			continue
		}
		if !add(p.CID) {
			break
		}
		if p.Mode != store.Recursive || p.Profile != "nu-tree" || p.Source == store.SourceFollow {
			continue
		}
		_, err := st.Walk(d.ctx, p, func(c cid.CID, held bool) error {
			if !held || c == p.CID {
				return nil
			}
			announce := c.Codec() == cid.Raw
			if c.Codec() == cid.DRISL {
				announce = isFileNode(st, c)
			}
			if announce && !add(c) {
				return errFull
			}
			return nil
		})
		if errors.Is(err, errFull) {
			break
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func isFileNode(st *store.Store, c cid.CID) bool {
	data, err := st.Get(c)
	if err != nil {
		return false
	}
	v, err := drisl.Decode(data)
	if err != nil {
		return false
	}
	m, ok := v.(map[string]any)
	return ok && m["$type"] == tree.TypeMASL
}

// dhtProviders is the fetcher's Amino source: it waits for bootstrap, then asks the
// DHT for provider records (fetch.Kademlia installs them as session hints).
type dhtProviders struct{ s *Swarm }

func (p dhtProviders) Find(ctx context.Context, c cid.CID, found func(fetch.Peer)) error {
	d := p.s.dht
	if p.s.DHT() == nil {
		return nil
	}
	if err := d.Ready(ctx); err != nil {
		return err
	}
	started, n := time.Now(), 0
	err := fetch.Kademlia{DHT: d.svc, Sessions: p.s.sessions}.Find(ctx, c, func(peer fetch.Peer) { n++; found(peer) })
	p.s.log.Info("DHT provider lookup", "cid", c, "providers", n, "elapsed", time.Since(started).Round(time.Millisecond), "err", err)
	return err
}

// DHTStatus reports the DHT section of the node status.
func (s *Swarm) DHTStatus() *apiv0.DHTStatus {
	out := &apiv0.DHTStatus{Mode: "off"}
	d := s.dht
	if s.DHT() == nil {
		return out
	}
	stats := d.svc.Stats()
	out.Mode = "client"
	if stats.Server {
		out.Mode = "server"
	}
	out.RoutingPeers = stats.RoutingPeers
	d.mu.Lock()
	defer d.mu.Unlock()
	out.Bootstrapped, out.BootstrapErr = d.bootstrapped, d.bootstrapErr
	out.LastProvide, out.ProvideErr, out.Provided = d.lastProvide, d.provideErr, d.provided
	for _, a := range d.announcing {
		out.Announcing = append(out.Announcing, a.String())
	}
	return out
}
