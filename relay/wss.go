package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wss"
	"golang.org/x/net/netutil"
)

// WSSConfig is an additional TCP listener, never a replacement for QUIC. The
// operator must advertise a matching admitted DNS/TCP/TLS/WS endpoint in
// Server.Addresses. TLS certificates use ordinary Web PKI, not libp2p TLS.
type WSSConfig struct {
	Listen         string
	TLS            *tls.Config
	MaxConnections int // default 128, includes unauthenticated TCP/TLS/HTTP
}
type wssListener struct {
	listener net.Listener
	server   *http.Server
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	conns    map[*trackedConn]struct{}
	closed   bool
	done     chan struct{}
}
type trackedConn struct {
	net.Conn
	owner *wssListener
	once  sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.conns, c); c.owner.mu.Unlock() })
	return err
}

type trackingListener struct{ *wssListener }

func (l trackingListener) Accept() (net.Conn, error) {
	raw, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	c := &trackedConn{Conn: raw, owner: l.wssListener}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		raw.Close()
		return nil, net.ErrClosed
	}
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return c, nil
}
func (l trackingListener) Addr() net.Addr { return l.listener.Addr() }
func (l trackingListener) Close() error   { return l.listener.Close() }
func newWSS(h *host.Host, cfg WSSConfig) (*wssListener, error) {
	if cfg.Listen == "" || cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 && cfg.TLS.GetCertificate == nil {
		return nil, errors.New("relay: WSS requires explicit TCP listen address and TLS certificate")
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 128
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > 4096 {
		return nil, errors.New("relay: invalid WSS connection limit")
	}
	raw, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &wssListener{listener: netutil.LimitListener(raw, cfg.MaxConnections), ctx: ctx, cancel: cancel, conns: make(map[*trackedConn]struct{}), done: make(chan struct{})}
	tlsConfig := cfg.TLS.Clone()
	tlsConfig.MinVersion = max(tlsConfig.MinVersion, tls.VersionTLS12)
	tlsConfig.NextProtos = []string{"http/1.1"}
	l.server = &http.Server{TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: wss.MaxHeaderBytes, TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler))}
	l.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		raw, err := wss.Upgrade(w, r)
		if err != nil {
			return
		}
		_, _ = h.AcceptWSS(l.ctx, raw)
	})
	go func() { defer close(l.done); _ = l.server.ServeTLS(trackingListener{l}, "", "") }()
	return l, nil
}
func (l *wssListener) Close() {
	l.cancel()
	l.mu.Lock()
	l.closed = true
	conns := make([]*trackedConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	l.server.Close()
	l.listener.Close()
	for _, c := range conns {
		c.Close()
	}
	<-l.done
}

// WSSAddr reports the actual bound TCP listener, or nil when WSS is disabled.
func (s *Server) WSSAddr() net.Addr {
	if s.wss == nil {
		return nil
	}
	return s.wss.listener.Addr()
}
