package host

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	quic "github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var ErrLimit = errors.New("host: resource limit reached")
var ErrNoConnection = errors.New("host: peer not connected")
var ErrUnsupportedAddress = errors.New("host: direct dial requires /ip4|ip6/.../udp/.../quic-v1/p2p/<id>; DNS and relay resolution are not available")

// Limits are hard local bounds, including handshakes in MaxConnections. Zero
// selects the documented defaults. Stream slots last until Close or Reset.
type Limits struct {
	MaxConnections, MaxConnectionsPerPeer                  int
	MaxStreams, MaxStreamsPerPeer, MaxStreamsPerConnection int
}

func (l Limits) defaults() (Limits, error) {
	values := []*int{&l.MaxConnections, &l.MaxConnectionsPerPeer, &l.MaxStreams, &l.MaxStreamsPerPeer, &l.MaxStreamsPerConnection}
	defaults := []int{128, 1, 1024, 64, 64}
	for i, p := range values {
		if *p < 0 {
			return l, ErrLimit
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	return l, nil
}

// Config accepts either literal UDP listen addresses (host:port), or supplied
// PacketConns. There may be at most one socket per IP family. New takes ownership
// of supplied sockets, including on error, and passes them directly to quic-go
// so OOB-capable wrappers can retain UDP optimizations. With neither, New binds
// 127.0.0.1:0; daemon callers should configure their public listeners explicitly.
type Config struct {
	Key         *wire.NodeKey
	Listen      []string
	PacketConns []net.PacketConn
	Limits      Limits
	// ManualStreams disables automatic protocol routing for transport-level users.
	ManualStreams bool
	AllowPeer     ConnectionGater
}
type endpoint struct {
	socket    net.PacketConn
	transport *quic.Transport
	listener  *quic.Listener
	family    int
}
type Host struct {
	key           *wire.NodeKey
	limits        Limits
	tls           *tls.Config
	quic          *quic.Config
	endpoints     []*endpoint
	ctx           context.Context
	cancel        context.CancelFunc
	slots         chan struct{}
	accepted      []*Conn
	acceptReady   chan struct{}
	manualStreams bool
	handlers      map[string]*protocolHandler
	watchers      map[uint64]chan struct{}
	nextWatcher   uint64
	gater         ConnectionGater
	gaters        map[uint64]ConnectionGater
	nextGater     uint64
	gateVersion   uint64
	streamGate    StreamGate
	dialGate      ConnectionGater
	mu            sync.Mutex
	closed        bool
	peers         map[wire.PeerID][]*Conn
	peerStreams   map[wire.PeerID]int
	totalStreams  int
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
}

func New(cfg Config) (h *Host, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	h = &Host{key: cfg.Key, ctx: ctx, cancel: cancel, peers: make(map[wire.PeerID][]*Conn), peerStreams: make(map[wire.PeerID]int)}
	// Every supplied socket is owned as soon as New is called.
	for _, pc := range cfg.PacketConns {
		h.endpoints = append(h.endpoints, &endpoint{socket: pc})
	}
	defer func() {
		if err != nil {
			_ = h.Close()
		}
	}()
	if cfg.Key == nil {
		return h, errors.New("host: node key required")
	}
	h.limits, err = cfg.Limits.defaults()
	if err != nil {
		return h, err
	}
	h.slots = make(chan struct{}, h.limits.MaxConnections)
	h.acceptReady = make(chan struct{}, 1)
	h.manualStreams = cfg.ManualStreams
	h.gater = cfg.AllowPeer
	h.handlers = make(map[string]*protocolHandler)
	cert, err := wire.NewTLSCertificate(cfg.Key, time.Now())
	if err != nil {
		return h, err
	}
	h.tls = &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{wire.TLSALPN}, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, InsecureSkipVerify: true, SessionTicketsDisabled: true} // libp2p identity is verified below, not Web PKI.
	h.tls.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		key, err := wire.VerifyTLSCertificate(raw, "", time.Now(), 16<<10)
		if err != nil {
			return err
		}
		return h.checkPeer(key.PeerID())
	}
	h.quic = &quic.Config{Versions: []quic.Version{quic.Version1}, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: time.Minute, KeepAlivePeriod: 20 * time.Second, MaxIncomingStreams: int64(h.limits.MaxStreamsPerConnection), MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: 64 << 10, MaxStreamReceiveWindow: 1 << 20, InitialConnectionReceiveWindow: 256 << 10, MaxConnectionReceiveWindow: 4 << 20}
	listen := cfg.Listen
	if len(listen) == 0 && len(cfg.PacketConns) == 0 {
		listen = []string{"127.0.0.1:0"}
	}
	for _, s := range listen {
		addr, e := net.ResolveUDPAddr("udp", s)
		if e != nil {
			return h, e
		}
		if addr.IP == nil {
			return h, errors.New("host: listen IP must be explicit")
		}
		network := "udp6"
		if addr.IP.To4() != nil {
			network = "udp4"
		}
		pc, e := net.ListenUDP(network, addr)
		if e != nil {
			return h, e
		}
		h.endpoints = append(h.endpoints, &endpoint{socket: pc})
	}
	seen := make(map[int]bool)
	for _, e := range h.endpoints {
		if e.socket == nil {
			return h, errors.New("host: nil socket")
		}
		addr, ok := e.socket.LocalAddr().(*net.UDPAddr)
		if !ok || addr.IP == nil {
			return h, errors.New("host: UDP socket address required")
		}
		e.family = 6
		if addr.IP.To4() != nil {
			e.family = 4
		}
		if seen[e.family] {
			return h, errors.New("host: only one socket per address family")
		}
		seen[e.family] = true
		var reset quic.StatelessResetKey
		if _, err = rand.Read(reset[:]); err != nil {
			return h, err
		}
		e.transport = &quic.Transport{Conn: e.socket, StatelessResetKey: &reset, ConnContext: func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
			if err := h.reserveConnection(); err != nil {
				return nil, err
			}
			context.AfterFunc(ctx, h.releaseConnection)
			return ctx, nil
		}}
		e.listener, err = e.transport.Listen(h.tls, h.quic)
		if err != nil {
			return h, err
		}
		h.wg.Add(1)
		go h.acceptLoop(e)
	}
	return h, nil
}
func (h *Host) ID() wire.PeerID           { return h.key.Public().PeerID() }
func (h *Host) PublicKey() wire.PublicKey { return h.key.Public() }
func (h *Host) Addrs() []wire.Multiaddr {
	var addrs []wire.Multiaddr
	for _, e := range h.endpoints {
		a, err := UDPAddress(e.socket.LocalAddr())
		if err == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// UDPAddress converts an actual UDP endpoint into its QUIC-v1 multiaddr.
func UDPAddress(addr net.Addr) (wire.Multiaddr, error) {
	a, ok := addr.(*net.UDPAddr)
	if !ok {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	family := "ip6"
	ip := a.IP
	if v4 := ip.To4(); v4 != nil {
		family = "ip4"
		ip = v4
	}
	if ip == nil || a.Zone != "" {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	return wire.ParseMultiaddr(fmt.Sprintf("/%s/%s/udp/%d/quic-v1", family, ip.String(), a.Port), 1024)
}
func (h *Host) reserveConnection() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return net.ErrClosed
	}
	select {
	case h.slots <- struct{}{}:
		return nil
	default:
		return ErrLimit
	}
}
func (h *Host) releaseConnection() { <-h.slots }
func (h *Host) acceptLoop(e *endpoint) {
	defer h.wg.Done()
	for {
		qc, err := e.listener.Accept(h.ctx)
		if err != nil {
			return
		}
		c, err := h.addConnection(qc, false)
		if err != nil {
			_ = qc.CloseWithError(1, "connection refused")
			continue
		}
		h.mu.Lock()
		live := h.accepted[:0]
		for _, old := range h.accepted {
			if old.Context().Err() == nil {
				live = append(live, old)
			}
		}
		h.accepted = append(live, c)
		h.mu.Unlock()
		select {
		case h.acceptReady <- struct{}{}:
		default:
		}
	}
}

// Accept reports incoming authenticated connections. Automatic stream routing
// remains active unless ManualStreams was configured.
func (h *Host) Accept(ctx context.Context) (*Conn, error) {
	for {
		h.mu.Lock()
		var c *Conn
		if len(h.accepted) > 0 {
			c = h.accepted[0]
			h.accepted[0] = nil
			h.accepted = h.accepted[1:]
			if len(h.accepted) > 0 {
				select {
				case h.acceptReady <- struct{}{}:
				default:
				}
			}
		}
		h.mu.Unlock()
		if c != nil {
			if c.Context().Err() == nil {
				return c, nil
			}
			continue
		}
		select {
		case <-h.acceptReady:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, net.ErrClosed
		}
	}
}
func (h *Host) Connection(id wire.PeerID) (*Conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.peers[id] {
		if c.Context().Err() == nil {
			return c, true
		}
	}
	return nil, false
}
func (h *Host) addConnection(qc *quic.Conn, outbound bool) (*Conn, error) {
	certs := qc.ConnectionState().TLS.PeerCertificates
	raw := make([][]byte, len(certs))
	for i, c := range certs {
		raw[i] = c.Raw
	}
	key, err := wire.VerifyTLSCertificate(raw, "", time.Now(), 16<<10)
	if err != nil {
		return nil, err
	}
	c := &Conn{host: h, quic: qc, peer: key.PeerID(), key: key, outbound: outbound, streams: make(map[*Stream]struct{})}
	for {
		version, err := h.checkPeerVersion(c.peer)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		if h.gateVersion == version {
			break
		}
		h.mu.Unlock()
	}
	defer h.mu.Unlock()
	if h.closed {
		return nil, net.ErrClosed
	}
	if c.peer == h.ID() {
		return nil, errors.New("host: self connection")
	}
	list := h.peers[c.peer]
	// Ignore closed entries whose asynchronous cleanup hasn't run yet.
	live := list[:0]
	for _, old := range list {
		if old.Context().Err() == nil {
			live = append(live, old)
		}
	}
	list = live
	if len(list) == 0 {
		delete(h.peers, c.peer)
	} else {
		h.peers[c.peer] = list
	}
	if len(list) >= h.limits.MaxConnectionsPerPeer {
		// Consistent simultaneous-dial tie-break: lower PeerID keeps its outgoing
		// connection; higher PeerID keeps the matching incoming connection.
		preferred := outbound == (h.ID() < c.peer)
		if h.limits.MaxConnectionsPerPeer != 1 || !preferred || list[0].outbound == outbound {
			return nil, ErrLimit
		}
		_ = list[0].quic.CloseWithError(0, "duplicate connection")
		list = nil
	}
	h.peers[c.peer] = append(list, c)
	h.signalChangedLocked()
	h.wg.Add(1)
	go func() { defer h.wg.Done(); <-qc.Context().Done(); h.removeConnection(c) }()
	if !h.manualStreams {
		h.wg.Add(1)
		go c.routeStreams()
	}
	return c, nil
}
func (h *Host) removeConnection(c *Conn) {
	h.mu.Lock()
	list := h.peers[c.peer]
	for i, old := range list {
		if old == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(h.peers, c.peer)
	} else {
		h.peers[c.peer] = list
	}
	streams := make([]*Stream, 0, len(c.streams))
	for s := range c.streams {
		streams = append(streams, s)
	}
	h.mu.Unlock()
	for _, s := range streams {
		s.Reset()
	}
}

// Dial requires /ip4|ip6/.../udp/.../quic-v1/p2p/<id>. It reuses an existing
// authenticated connection. Address racing and discovery belong to the session layer.
func (h *Host) Dial(ctx context.Context, addr wire.Multiaddr) (*Conn, error) {
	remote, id, family, err := dialAddress(addr)
	if err != nil {
		return nil, err
	}
	if err := h.checkPeer(id); err != nil {
		return nil, err
	}
	if err := h.checkDial(id); err != nil {
		return nil, err
	}
	if c, ok := h.Connection(id); ok {
		return c, nil
	}
	var ep *endpoint
	for _, e := range h.endpoints {
		if e.family == family {
			ep = e
			break
		}
	}
	if ep == nil {
		return nil, errors.New("host: no socket for remote address family")
	}
	if err := h.reserveConnection(); err != nil {
		return nil, err
	}
	tlsConfig := h.tls.Clone()
	verificationError := make(chan error, 1)
	tlsConfig.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		_, err := wire.VerifyTLSCertificate(raw, id, time.Now(), 16<<10)
		if err == nil {
			err = h.checkPeer(id)
		}
		if err != nil {
			select {
			case verificationError <- err:
			default:
			}
		}
		return err
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	qc, err := ep.transport.Dial(dialCtx, remote, tlsConfig, h.quic)
	if err != nil {
		h.releaseConnection()
		select {
		case verification := <-verificationError:
			err = errors.Join(err, verification)
		default:
		}
		return nil, err
	}
	context.AfterFunc(qc.Context(), h.releaseConnection)
	c, err := h.addConnection(qc, true)
	if err != nil {
		_ = qc.CloseWithError(1, "connection refused")
		if existing, ok := h.Connection(id); ok {
			return existing, nil
		}
		return nil, err
	}
	return c, nil
}
func dialAddress(addr wire.Multiaddr) (*net.UDPAddr, wire.PeerID, int, error) {
	if addr.Opaque() {
		return nil, "", 0, ErrUnsupportedAddress
	}
	m, err := ma.NewMultiaddrBytes(addr.Bytes())
	if err != nil {
		return nil, "", 0, err
	}
	parts := ma.Split(m)
	if len(parts) != 4 || parts[1].Protocol().Code != ma.P_UDP || parts[2].Protocol().Code != ma.P_QUIC_V1 || parts[3].Protocol().Code != ma.P_P2P {
		return nil, "", 0, ErrUnsupportedAddress
	}
	family := 4
	switch parts[0].Protocol().Code {
	case ma.P_IP4:
	case ma.P_IP6:
		family = 6
	default:
		return nil, "", 0, ErrUnsupportedAddress
	}
	id, err := wire.ParsePeerID(parts[3].Value())
	if err != nil {
		return nil, "", 0, err
	}
	port, err := strconv.Atoi(parts[1].Value())
	if err != nil || port == 0 {
		return nil, "", 0, wire.ErrFormat
	}
	return &net.UDPAddr{IP: net.ParseIP(parts[0].Value()), Port: port}, id, family, nil
}
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.cancel()
		for _, list := range h.peers {
			for _, c := range list {
				_ = c.quic.CloseWithError(0, "host closed")
			}
		}
		h.mu.Unlock()
		for _, e := range h.endpoints {
			if e.listener != nil {
				_ = e.listener.Close()
			}
			if e.transport != nil {
				_ = e.transport.Close()
			}
			if e.socket != nil {
				if err := e.socket.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					h.closeErr = err
				}
			}
		}
		h.wg.Wait()
	})
	return h.closeErr
}
