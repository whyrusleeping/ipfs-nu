package relay

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/p2p/lookup"
)

// LookupConfig enables optional peer discovery surfaces. Empty listen addresses
// expose handlers only, for an owner-managed HTTPS/DNS frontend. HTTPListen is
// plaintext: terminate public TLS in front of it. No public listener is implicit.
type LookupConfig struct {
	Directory  lookup.DirectoryConfig
	HTTPListen string
	DNSListen  string // serves both UDP and TCP on the same port (including :0)
}
type LookupService struct {
	Directory    *lookup.Directory
	http         *http.Server
	httpListener net.Listener
	udp, tcp     *dns.Server
	errors       chan error
	once         sync.Once
	wg           sync.WaitGroup
}

func NewLookup(cfg LookupConfig) (*LookupService, error) {
	dir, err := lookup.NewDirectory(cfg.Directory)
	if err != nil {
		return nil, err
	}
	s := &LookupService{Directory: dir, errors: make(chan error, 3)}
	if cfg.HTTPListen != "" {
		s.httpListener, err = net.Listen("tcp", cfg.HTTPListen)
		if err != nil {
			return nil, err
		}
		s.http = &http.Server{Handler: dir, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.http.Serve(s.httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.errors <- err
			}
		}()
	}
	if cfg.DNSListen != "" {
		udp, tcp, err := listenDNS(cfg.DNSListen)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.udp = &dns.Server{PacketConn: udp, Net: "udp", Handler: dir, UDPSize: 1232}
		s.tcp = &dns.Server{Listener: tcp, Net: "tcp", Handler: dir, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, MaxTCPQueries: 32}
		for _, server := range []*dns.Server{s.udp, s.tcp} {
			ready := make(chan struct{})
			finished := make(chan error, 1)
			server.NotifyStartedFunc = func() { close(ready) }
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				err := server.ActivateAndServe()
				finished <- err
				if err != nil {
					s.errors <- err
				}
			}()
			select {
			case <-ready:
			case err := <-finished:
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *LookupService) HTTPAddr() net.Addr {
	if s.httpListener == nil {
		return nil
	}
	return s.httpListener.Addr()
}
func (s *LookupService) DNSAddr() net.Addr {
	if s.udp == nil {
		return nil
	}
	return s.udp.PacketConn.LocalAddr()
}
func (s *LookupService) Errors() <-chan error { return s.errors }
func (s *LookupService) Close() error {
	s.once.Do(func() {
		if s.http != nil {
			_ = s.http.Close()
		}
		if s.udp != nil {
			_ = s.udp.Shutdown()
			_ = s.udp.PacketConn.Close()
		}
		if s.tcp != nil {
			_ = s.tcp.Shutdown()
			_ = s.tcp.Listener.Close()
		}
		s.wg.Wait()
		close(s.errors)
	})
	return nil
}

// listenDNS binds UDP and TCP on the same port. With port 0 the kernel picks the
// UDP port, and that number can already be taken for TCP by an unrelated listener,
// so it retries with a fresh port a few times; a fixed port fails immediately.
func listenDNS(addr string) (net.PacketConn, net.Listener, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, err
	}
	attempts := 1
	if port == "0" {
		attempts = 16
	}
	for i := 0; ; i++ {
		udp, err := net.ListenPacket("udp", addr)
		if err != nil {
			return nil, nil, err
		}
		tcp, err := net.Listen("tcp", udp.LocalAddr().String())
		if err == nil {
			return udp, tcp, nil
		}
		udp.Close()
		if i+1 >= attempts {
			return nil, nil, err
		}
	}
}
