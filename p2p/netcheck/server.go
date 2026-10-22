package netcheck

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// ServerSet binds one family's observer sockets. Advertise defaults to Listen;
// set it when the observer sits behind 1:1 NAT (role layout must match).
type ServerSet struct {
	Listen, Advertise EndpointSet
}

// ServerConfig configures an observer (served by `nu relay`). Defaults: cookies
// valid 10 minutes; replies limited to 20/s per source IP (burst 40) and
// 2000/s globally (burst 4000). Replies are never larger than requests.
type ServerConfig struct {
	Sets                         []ServerSet
	CookieLifetime               time.Duration
	PerIPPerSecond, PerIPBurst   int
	GlobalPerSecond, GlobalBurst int
}

const (
	maxCookieLifetime = time.Hour
	maxRateEntries    = 4096
	cookieLabel       = "nu-netcheck-1 cookie"
)

type serverSocket struct {
	conn *net.UDPConn
	set  int
	role Role
}
type Server struct {
	cfg     ServerConfig
	desc    Descriptor
	key     [32]byte
	sockets []*serverSocket
	byRole  [][4]*serverSocket
	lim     rateLimiter
	wg      sync.WaitGroup
	once    sync.Once
	now     func() time.Time
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.CookieLifetime == 0 {
		cfg.CookieLifetime = 10 * time.Minute
	}
	defaults := []*int{&cfg.PerIPPerSecond, &cfg.PerIPBurst, &cfg.GlobalPerSecond, &cfg.GlobalBurst}
	for i, v := range []int{20, 40, 2000, 4000} {
		if *defaults[i] < 0 {
			return nil, errors.New("netcheck: negative rate limit")
		}
		if *defaults[i] == 0 {
			*defaults[i] = v
		}
	}
	if cfg.CookieLifetime < time.Minute || cfg.CookieLifetime > maxCookieLifetime {
		return nil, errors.New("netcheck: cookie lifetime must be 1m-1h")
	}
	s := &Server{cfg: cfg, now: time.Now, lim: rateLimiter{perIP: make(map[netip.Addr]*bucket)}}
	s.lim.global = bucket{tokens: float64(cfg.GlobalBurst)}
	for _, set := range cfg.Sets {
		adv := set.Advertise
		if !adv.Primary.IsValid() {
			adv = set.Listen
		}
		if err := set.Listen.Validate(); err != nil {
			return nil, err
		}
		if adv.AltPort.IsValid() != set.Listen.AltPort.IsValid() || adv.AltIP.IsValid() != set.Listen.AltIP.IsValid() {
			return nil, errors.New("netcheck: advertised roles must match listened roles")
		}
		s.desc.Sets = append(s.desc.Sets, adv)
	}
	if err := s.desc.Validate(); err != nil {
		return nil, err
	}
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	s.byRole = make([][4]*serverSocket, len(cfg.Sets))
	for i, set := range cfg.Sets {
		for r := RolePrimary; r <= RoleAltIP; r++ {
			ep := set.Listen.endpoint(r)
			if !ep.IsValid() {
				continue
			}
			conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ep))
			if err != nil {
				s.Close()
				return nil, fmt.Errorf("netcheck: listen %s: %w", ep, err)
			}
			sock := &serverSocket{conn: conn, set: i, role: r}
			s.sockets = append(s.sockets, sock)
			s.byRole[i][r] = sock
		}
	}
	for _, sock := range s.sockets {
		s.wg.Add(1)
		go s.serve(sock)
	}
	return s, nil
}

// Descriptor is what /nu/netcheck/1 returns.
func (s *Server) Descriptor() Descriptor { return s.desc }

func (s *Server) Close() error {
	s.once.Do(func() {
		for _, sock := range s.sockets {
			sock.conn.Close()
		}
	})
	s.wg.Wait()
	return nil
}

func (s *Server) serve(sock *serverSocket) {
	defer s.wg.Done()
	buf := make([]byte, PacketSize+1)
	for {
		n, src, err := sock.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.handle(sock, buf[:n], netip.AddrPortFrom(src.Addr().Unmap(), src.Port()))
	}
}

// handle silently drops anything malformed, unauthorised or over budget.
func (s *Server) handle(sock *serverSocket, b []byte, src netip.AddrPort) {
	req, err := parsePacket(b)
	if err != nil || req.Type != typeRequest || !usable(src) {
		return
	}
	now := s.now()
	if req.Flags != 0 || req.TargetPort != 0 {
		if !s.validCookie(req.Cookie, src.Addr(), now) {
			return
		}
	}
	from := sock
	if req.Flags != 0 {
		if sock.role != RolePrimary {
			return
		}
		role := RoleAltPort
		if req.Flags == FlagAltIP {
			role = RoleAltIP
		}
		if from = s.byRole[sock.set][role]; from == nil {
			return
		}
	}
	dst := src
	if req.TargetPort != 0 {
		dst = netip.AddrPortFrom(src.Addr(), req.TargetPort)
	}
	if !s.lim.allow(src.Addr(), now, s.cfg) {
		return
	}
	resp := packet{Type: typeResponse, TxID: req.TxID, Cookie: s.cookie(src.Addr(), now), Observed: src, Role: from.role}
	out := resp.marshal()
	_, _ = from.conn.WriteToUDPAddrPort(out[:], dst)
}

func (s *Server) cookieMAC(expiry uint32, ip netip.Addr) []byte {
	m := hmac.New(sha256.New, s.key[:])
	m.Write([]byte(cookieLabel))
	var b [20]byte
	binary.BigEndian.PutUint32(b[:], expiry)
	a := ip.Unmap().As16()
	copy(b[4:], a[:])
	m.Write(b[:])
	return m.Sum(nil)[:12]
}

// Cookie: expiry unix seconds (u32) | first 12 bytes of HMAC-SHA256(key,
// "nu-netcheck-1 cookie" | expiry | ip16). It proves the requester received a
// reply at that IP (return routability); it never names a port.
func (s *Server) cookie(ip netip.Addr, now time.Time) (c [16]byte) {
	expiry := uint32(now.Add(s.cfg.CookieLifetime).Unix())
	binary.BigEndian.PutUint32(c[:], expiry)
	copy(c[4:], s.cookieMAC(expiry, ip))
	return c
}
func (s *Server) validCookie(c [16]byte, ip netip.Addr, now time.Time) bool {
	expiry := binary.BigEndian.Uint32(c[:])
	t := time.Unix(int64(expiry), 0)
	if t.Before(now) || t.After(now.Add(s.cfg.CookieLifetime+time.Second)) {
		return false
	}
	return hmac.Equal(c[4:], s.cookieMAC(expiry, ip))
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) take(now time.Time, rate, burst int) bool {
	if !b.last.IsZero() {
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.last).Seconds()*float64(rate))
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type rateLimiter struct {
	mu     sync.Mutex
	global bucket
	perIP  map[netip.Addr]*bucket
}

func (l *rateLimiter) allow(ip netip.Addr, now time.Time, cfg ServerConfig) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.perIP[ip]
	if b == nil {
		if len(l.perIP) >= maxRateEntries {
			for k, v := range l.perIP {
				if now.Sub(v.last).Seconds()*float64(cfg.PerIPPerSecond) >= float64(cfg.PerIPBurst) {
					delete(l.perIP, k)
				}
			}
			if len(l.perIP) >= maxRateEntries {
				return false
			}
		}
		b = &bucket{tokens: float64(cfg.PerIPBurst)}
	}
	if !b.take(now, cfg.PerIPPerSecond, cfg.PerIPBurst) {
		return false
	}
	l.perIP[ip] = b
	return l.global.take(now, cfg.GlobalPerSecond, cfg.GlobalBurst)
}

// Register serves the descriptor on /nu/netcheck/1. The request is the single
// byte 0x01 (wire version); the reply is the encoded Descriptor, then EOF.
func (s *Server) Register(h *host.Host) error {
	desc, err := s.desc.MarshalBinary()
	if err != nil {
		return err
	}
	return h.Handle(ProtocolID, 16, func(st *host.Stream) {
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		var v [1]byte
		if _, err := io.ReadFull(st, v[:]); err != nil || v[0] != 1 {
			st.Reset()
			return
		}
		if _, err := st.Write(desc); err != nil {
			st.Reset()
		}
	})
}

// Fetch asks a connected peer for its observer descriptor.
func Fetch(ctx context.Context, h *host.Host, peer wire.PeerID) (Descriptor, error) {
	st, err := h.OpenStream(ctx, peer, ProtocolID)
	if err != nil {
		return Descriptor{}, err
	}
	defer st.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = st.SetDeadline(deadline)
	if _, err = st.Write([]byte{1}); err != nil {
		st.Reset()
		return Descriptor{}, err
	}
	_ = st.CloseWrite()
	b, err := io.ReadAll(io.LimitReader(st, 2+3*maxSets*(1+endpointSize)+1))
	if err != nil {
		st.Reset()
		return Descriptor{}, err
	}
	return DecodeDescriptor(b)
}

// ServeProfiles answers /nu/natprofile/1: request max_version:u8 (>=1), reply
// len:u8 | profile (len 0: no profile yet), then EOF. The profile is
// address-free; owners decide which peers may connect (private mode members).
func ServeProfiles(h *host.Host, current func() (Profile, bool)) error {
	return h.Handle(ProfileProtocolID, 16, func(st *host.Stream) {
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		var v [1]byte
		if _, err := io.ReadFull(st, v[:]); err != nil || v[0] == 0 {
			st.Reset()
			return
		}
		out := []byte{0}
		if p, ok := current(); ok {
			if enc, err := p.AppendBinary(out); err == nil {
				enc[0] = byte(len(enc) - 1)
				out = enc
			}
		}
		if _, err := st.Write(out); err != nil {
			st.Reset()
		}
	})
}

// FetchProfile retrieves a peer's current profile (ok=false: none published).
func FetchProfile(ctx context.Context, h *host.Host, peer wire.PeerID) (Profile, bool, error) {
	st, err := h.OpenStream(ctx, peer, ProfileProtocolID)
	if err != nil {
		return Profile{}, false, err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = st.Write([]byte{ProfileVersion}); err != nil {
		st.Reset()
		return Profile{}, false, err
	}
	_ = st.CloseWrite()
	b, err := io.ReadAll(io.LimitReader(st, 1+255))
	if err != nil {
		st.Reset()
		return Profile{}, false, err
	}
	if len(b) == 0 || int(b[0]) != len(b)-1 {
		return Profile{}, false, fmt.Errorf("%w: profile framing", ErrProfile)
	}
	if b[0] == 0 {
		return Profile{}, false, nil
	}
	p, err := DecodeProfile(b[1:])
	return p, err == nil, err
}
