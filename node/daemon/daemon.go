// Package daemon assembles and runs the Nu daemon: it opens the repo, takes the daemon
// lock, starts the swarm (node/swarm), serves the local API and the loopback gateway,
// records their addresses in the repo so clients can find them, and shuts down gracefully.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/follow"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/swarm"
)

// Options configures a daemon.
type Options struct {
	// Repo is the opened repo (required).
	Repo *repo.Repo
	// APIListen and GatewayListen override config.toml when non-empty. ":0" picks a port.
	APIListen, GatewayListen string
	// SwarmListen overrides swarm.listen when non-empty (multiaddrs).
	SwarmListen []string
	// ProducerListen overrides api.producer.listen when non-empty (tests).
	ProducerListen string
	// NoNetwork runs without a swarm (no fetching, no serving); NoMDNS keeps the swarm
	// but disables LAN discovery. Swarm customizes the swarm further (tests).
	NoNetwork, NoMDNS bool
	Swarm             func(*swarm.Config)
	Logger            *slog.Logger
	// Atproto replaces the node's atproto client configuration (tests: fixtures).
	Atproto *atproto.Config
}

// Daemon is a running daemon.
type Daemon struct {
	Node  *node.Local
	Swarm *swarm.Swarm // nil with NoNetwork

	log     *slog.Logger
	lock    *repo.Lock
	apiLn   net.Listener
	gwLn    net.Listener
	apiSrv  *http.Server
	gwSrv   *http.Server
	errs    chan error
	closeMu sync.Mutex
	closed  bool

	maintainCtx  context.Context
	maintainStop context.CancelFunc
	maintainDone chan struct{}

	followDone chan struct{}
	// The producer listener (api.producer.*, rule A8), when configured.
	prodLn  net.Listener
	prodSrv *http.Server
	// The public content listener ([public], rules P1–P6), when configured.
	pubLn  net.Listener
	pubSrv *http.Server
}

// ShutdownTimeout bounds graceful shutdown of the HTTP servers.
const ShutdownTimeout = 5 * time.Second

// Start binds the listeners, writes the address files and begins serving. It fails with
// a conflict error when another daemon holds the repo lock.
func Start(ctx context.Context, opts Options) (*Daemon, error) {
	if opts.Repo == nil {
		return nil, errors.New("daemon: Options.Repo is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg, err := opts.Repo.Config()
	if err != nil {
		return nil, err
	}
	apiAddr, gwAddr := cfg.API.Listen, cfg.Gateway.Listen
	if opts.APIListen != "" {
		apiAddr = opts.APIListen
	}
	if opts.GatewayListen != "" {
		gwAddr = opts.GatewayListen
	}
	for name, a := range map[string]string{"api.listen": apiAddr, "gateway.listen": gwAddr} {
		if err := requireLoopback(a); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s = %q: %v", name, a, err).
				WithAction("non-loopback listeners need authentication, which this build does not have; use 127.0.0.1 or localhost")
		}
	}

	lock, err := opts.Repo.Lock()
	if err != nil {
		if errors.Is(err, repo.ErrLocked) {
			return nil, apiv0.Errorf(apiv0.CodeConflict, "%v", err).
				WithAction("stop the other daemon first, or use a different " + repo.EnvPath)
		}
		return nil, err
	}
	n, err := node.NewLocal(opts.Repo)
	if err != nil {
		lock.Release()
		return nil, err
	}
	if opts.Atproto != nil {
		n.Atproto = atproto.New(*opts.Atproto)
	}
	d := &Daemon{Node: n, log: log, lock: lock, errs: make(chan error, 4)}
	if !opts.NoNetwork {
		scfg, err := swarm.RepoConfig(opts.Repo, n.Key, n.Store)
		if err != nil {
			d.Close()
			return nil, err
		}
		scfg.Logger = log
		if len(opts.SwarmListen) > 0 {
			scfg.Listen = opts.SwarmListen
		}
		if opts.NoMDNS {
			scfg.MDNS = false
		}
		if opts.Swarm != nil {
			opts.Swarm(&scfg)
		}
		// Cluster gating applies from the moment the host listens; private and isolated
		// clusters run no LAN discovery.
		scfg.AllowPeer = n.GateSwarm(scfg.AllowPeer)
		if n.PrivateNetwork() {
			scfg.Isolated = true // cluster gate owns membership; disable all public mechanisms
			scfg.MDNS = false
		}
		d.Swarm, err = swarm.Start(ctx, scfg)
		if err != nil {
			d.Close()
			return nil, err
		}
		n.Fetcher, n.Net = d.Swarm, d.Swarm
		if err := n.AttachNetwork(d.Swarm); err != nil {
			d.Close()
			return nil, err
		}
	}

	d.apiLn, err = net.Listen("tcp", apiAddr)
	if err != nil {
		d.Close()
		return nil, listenError("API", apiAddr, err)
	}
	d.gwLn, err = net.Listen("tcp", gwAddr)
	if err != nil {
		d.Close()
		return nil, listenError("gateway", gwAddr, err)
	}
	d.Node.API = d.apiLn.Addr().String()
	d.Node.Gateway = d.gwLn.Addr().String()

	if err := opts.Repo.WriteAddrFile(repo.APIFile, "http://"+d.Node.API); err != nil {
		d.Close()
		return nil, err
	}
	if err := opts.Repo.WriteAddrFile(repo.GatewayFile, "http://"+d.Node.Gateway); err != nil {
		d.Close()
		return nil, err
	}

	d.apiSrv = &http.Server{
		Handler:           api.New(d.Node, log),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	d.gwSrv = &http.Server{
		Handler:           gateway.New(d.Node.Content),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go d.serve(d.apiSrv, d.apiLn)
	go d.serve(d.gwSrv, d.gwLn)
	if err := d.startProducer(ctx, opts); err != nil {
		d.Close()
		return nil, err
	}
	if err := d.startPublic(ctx, cfg); err != nil {
		d.Close()
		return nil, err
	}
	// Store maintenance (D17 A7): GC at the watermark or below the free-space reserve.
	d.maintainCtx, d.maintainStop = context.WithCancel(ctx)
	d.maintainDone = make(chan struct{})
	go func() {
		defer close(d.maintainDone)
		n.Maintain(d.maintainCtx, log.Info)
	}()
	// Follows (0045): re-resolve on startup, then the poll backstop; optionally the
	// Jetstream hint source restricted to the followed DIDs.
	if fc := cfg.Follow; fc != nil {
		if fc.Poll != "" {
			if p, err := time.ParseDuration(fc.Poll); err == nil && p > 0 {
				node.FollowPoll = p
			}
		}
		if fc.Jetstream != "" {
			n.SetJetstream(&follow.Jetstream{URL: fc.Jetstream})
		}
	}
	d.followDone = make(chan struct{})
	go func() {
		defer close(d.followDone)
		n.FollowLoop(d.maintainCtx)
	}()
	log.Info("daemon started", "version", nu.Version, "repo", opts.Repo.Root(), "api", d.Node.API, "gateway", d.Node.Gateway, "peer", n.PeerID().Base36())
	return d, nil
}

func (d *Daemon) serve(srv *http.Server, ln net.Listener) {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	d.errs <- err
}

// APIAddr and GatewayAddr return the bound host:port addresses.
func (d *Daemon) APIAddr() string     { return d.Node.API }
func (d *Daemon) GatewayAddr() string { return d.Node.Gateway }

// Wait blocks until ctx ends or a listener fails, then shuts the daemon down and returns
// the failure, if any.
func (d *Daemon) Wait(ctx context.Context) error {
	var err error
	select {
	case <-ctx.Done():
	case err = <-d.errs:
	}
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close shuts down gracefully: stops accepting, cancels running operations, lets handlers
// finish within ShutdownTimeout, closes the node (waiting for operations, then the store),
// removes the address files and releases the lock.
func (d *Daemon) Close() error {
	d.closeMu.Lock()
	defer d.closeMu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.Node != nil {
		d.Node.Ops.CancelAll()
	}
	if d.maintainStop != nil {
		d.maintainStop()
		<-d.maintainDone
		if d.followDone != nil {
			<-d.followDone
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	for _, srv := range []*http.Server{d.apiSrv, d.gwSrv, d.prodSrv, d.pubSrv} {
		if srv != nil {
			keep(srv.Shutdown(ctx))
		}
	}
	for _, ln := range []net.Listener{d.apiLn, d.gwLn, d.prodLn, d.pubLn} {
		if ln != nil {
			_ = ln.Close()
		}
	}
	if d.Node != nil {
		d.Node.Ops.CancelAll()
		d.Node.Wait()
	}
	if d.Swarm != nil {
		d.Node.DetachNetwork()
		keep(d.Swarm.Close())
	}
	if d.Node != nil {
		keep(d.Node.Close())
		keep(d.Node.Repo.RemoveAddrFile(repo.APIFile))
		keep(d.Node.Repo.RemoveAddrFile(repo.GatewayFile))
	}
	keep(d.lock.Release())
	if d.log != nil {
		d.log.Info("daemon stopped")
	}
	return firstErr
}

// Run starts a daemon and serves until ctx ends.
func Run(ctx context.Context, opts Options) error {
	d, err := Start(ctx, opts)
	if err != nil {
		return err
	}
	return d.Wait(ctx)
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("not a loopback address")
	}
	return nil
}

func listenError(what, addr string, err error) error {
	return apiv0.Errorf(apiv0.CodeUnavailable, "cannot listen for the %s on %s: %v", what, addr, err).
		WithAction(fmt.Sprintf("if another program uses the port, change it with `%s config set %s.listen 127.0.0.1:<port>`",
			nu.Name, map[string]string{"API": "api", "gateway": "gateway"}[what]))
}
