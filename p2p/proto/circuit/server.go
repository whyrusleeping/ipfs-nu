package circuit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// ServerConfig defaults to ordinary limited public circuits. Only when BOTH
// endpoints are explicitly Admitted is FullLimit applied. Resource slot/IP/peer
// caps still apply. This is a development allow-list, not production admission.
type ServerConfig struct {
	ReservationTTL, Timeout                                 time.Duration
	MaxReservations, ReservationsPerIP                      int
	MaxCircuits, CircuitsPerPeer, CircuitsPerIP, MaxPending int
	Limited, FullLimit                                      Limit
	Admitted                                                []wire.PeerID
	// AllowPeer optionally refuses requests/reservations from an identity/IP.
	// The callback is concurrent and must not block; nil permits limited service.
	AllowPeer func(wire.PeerID, netip.Addr) bool
	// Addresses are public direct relay addresses including /p2p/<relay>.
	// Empty uses the host's explicit listen addresses. Wildcards are rejected.
	Addresses []wire.Multiaddr
}
type reservation struct {
	expires time.Time
	conn    *host.Conn
	ip      netip.Addr
}
type Server struct {
	host         *host.Host
	key          *wire.NodeKey
	cfg          ServerConfig
	admitted     map[wire.PeerID]bool
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	reservations map[wire.PeerID]reservation
	active       int
	byPeer       map[wire.PeerID]int
	byIP         map[netip.Addr]int
	pending      chan struct{}
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

func NewServer(h *host.Host, key *wire.NodeKey, cfg ServerConfig) (*Server, error) {
	if h == nil || h.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	if key == nil || key.Public().PeerID() != h.ID() {
		return nil, wire.ErrSignature
	}
	if cfg.ReservationTTL < 0 || cfg.Timeout < 0 || cfg.MaxReservations < 0 || cfg.ReservationsPerIP < 0 || cfg.MaxCircuits < 0 || cfg.CircuitsPerPeer < 0 || cfg.CircuitsPerIP < 0 || cfg.MaxPending < 0 {
		return nil, wire.ErrLimit
	}
	if cfg.ReservationTTL == 0 {
		cfg.ReservationTTL = time.Hour
	}
	if cfg.ReservationTTL < time.Second || cfg.ReservationTTL > 24*time.Hour {
		return nil, wire.ErrLimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxReservations == 0 {
		cfg.MaxReservations = 128
	}
	if cfg.ReservationsPerIP == 0 {
		cfg.ReservationsPerIP = 8
	}
	if cfg.MaxCircuits == 0 {
		cfg.MaxCircuits = 128
	}
	if cfg.CircuitsPerPeer == 0 {
		cfg.CircuitsPerPeer = 8
	}
	if cfg.CircuitsPerIP == 0 {
		cfg.CircuitsPerIP = 16
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 32
	}
	if cfg.Limited == (Limit{}) {
		cfg.Limited = Limit{Duration: 120, Data: 128 << 10}
	}
	// Public non-admitted circuits must never accidentally be unlimited.
	if cfg.Limited.Duration == 0 || cfg.Limited.Data == 0 {
		return nil, wire.ErrLimit
	}
	if len(cfg.Admitted) > 1024 || len(cfg.Addresses) > MaxAddresses {
		return nil, wire.ErrLimit
	}
	for _, p := range h.Protocols() {
		if p == HopProtocol {
			return nil, errors.New("circuit: HOP already registered")
		}
	}
	if len(cfg.Addresses) == 0 {
		for _, a := range h.Addrs() {
			addr, err := wire.ParseMultiaddr(a.String()+"/p2p/"+h.ID().String(), 1024)
			if err != nil {
				return nil, err
			}
			cfg.Addresses = append(cfg.Addresses, addr)
		}
	}
	for _, a := range cfg.Addresses {
		if err := relayAddress(a, h.ID()); err != nil {
			return nil, err
		}
	}
	// Ensure the largest response fits the fixed protocol frame before registering.
	voucher, err := SealVoucher(key, h.ID(), time.Now().Add(cfg.ReservationTTL))
	if err != nil {
		return nil, err
	}
	if len(marshal(message{kind: 2, status: OK, reservation: &Reservation{Expires: time.Now().Add(cfg.ReservationTTL), Addresses: cfg.Addresses, Voucher: voucher}, limit: cfg.Limited}, false)) > MaxMessage {
		return nil, wire.ErrLimit
	}
	ctx, cancel := context.WithCancel(h.Context())
	s := &Server{host: h, key: key, cfg: cfg, admitted: make(map[wire.PeerID]bool), ctx: ctx, cancel: cancel, reservations: make(map[wire.PeerID]reservation), byPeer: make(map[wire.PeerID]int), byIP: make(map[netip.Addr]int), pending: make(chan struct{}, cfg.MaxPending)}
	for _, id := range cfg.Admitted {
		if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
			cancel()
			return nil, err
		}
		s.admitted[id] = true
	}
	if err = h.Handle(HopProtocol, cfg.MaxPending+cfg.MaxCircuits, s.handle); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}
func (s *Server) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	s.wg.Add(1)
	return true
}
func (s *Server) Close() error {
	s.closeOnce.Do(func() { s.mu.Lock(); s.cancel(); s.mu.Unlock(); s.host.RemoveHandler(HopProtocol); s.wg.Wait() })
	return nil
}
func (s *Server) pruneLocked() {
	now := time.Now()
	for id, r := range s.reservations {
		if !r.expires.After(now) || r.conn.Context().Err() != nil {
			delete(s.reservations, id)
		}
	}
}
func remoteIP(conn *host.Conn) (netip.Addr, bool) {
	switch addr := conn.RemoteAddr().(type) {
	case *net.UDPAddr:
		return addr.AddrPort().Addr().Unmap(), true
	case *net.TCPAddr:
		return addr.AddrPort().Addr().Unmap(), true
	default:
		return netip.Addr{}, false
	}
}
func (s *Server) permitted(conn *host.Conn) bool {
	ip, ok := remoteIP(conn)
	return ok && (s.cfg.AllowPeer == nil || s.cfg.AllowPeer(conn.PeerID(), ip))
}
func (s *Server) handle(stream *host.Stream) {
	if !s.begin() {
		stream.Reset()
		return
	}
	defer s.wg.Done()
	stopService := context.AfterFunc(s.ctx, stream.Reset)
	defer stopService()
	_ = stream.SetDeadline(time.Now().Add(s.cfg.Timeout))
	select {
	case s.pending <- struct{}{}:
	default:
		_ = writeMessage(stream, message{kind: 2, status: ResourceLimit}, false)
		return
	}
	pending := true
	release := func() {
		if pending {
			<-s.pending
			pending = false
		}
	}
	defer release()
	m, err := readMessage(stream, false)
	if err != nil {
		_ = writeMessage(stream, message{kind: 2, status: MalformedMessage}, false)
		return
	}
	if !s.permitted(stream.Conn()) {
		_ = writeMessage(stream, message{kind: 2, status: PermissionDenied}, false)
		return
	}
	switch m.kind {
	case 0:
		s.reserve(stream)
	case 1:
		s.connect(stream, m.peer, release)
	default:
		_ = writeMessage(stream, message{kind: 2, status: UnexpectedMessage}, false)
	}
}
func (s *Server) reserve(stream *host.Stream) {
	conn := stream.Conn()
	id := conn.PeerID()
	ip, _ := remoteIP(conn)
	s.mu.Lock()
	s.pruneLocked()
	_, exists := s.reservations[id]
	count := 0
	for peer, r := range s.reservations {
		if peer != id && r.ip == ip {
			count++
		}
	}
	if (!exists && len(s.reservations) >= s.cfg.MaxReservations) || count >= s.cfg.ReservationsPerIP {
		s.mu.Unlock()
		_ = writeMessage(stream, message{kind: 2, status: ReservationRefused}, false)
		return
	}
	// Round up to avoid very short TTLs being expired by Unix-second truncation.
	expires := time.Unix(time.Now().Add(s.cfg.ReservationTTL).Unix()+1, 0)
	voucher, err := SealVoucher(s.key, id, expires)
	if err != nil {
		s.mu.Unlock()
		stream.Reset()
		return
	}
	s.reservations[id] = reservation{expires: expires, conn: conn, ip: ip}
	s.mu.Unlock()
	limit := s.cfg.Limited
	if s.admitted[id] {
		limit = s.cfg.FullLimit
	}
	if err = writeMessage(stream, message{kind: 2, status: OK, reservation: &Reservation{Expires: expires, Addresses: s.cfg.Addresses, Voucher: voucher}, limit: limit}, false); err != nil {
		stream.Reset()
	}
}
func (s *Server) connect(in *host.Stream, target wire.PeerID, releasePending func()) {
	fail := func(status Status) { _ = writeMessage(in, message{kind: 2, status: status}, false) }
	if target == "" {
		fail(MalformedMessage)
		return
	}
	source := in.Conn().PeerID()
	if target == source || target == s.host.ID() {
		fail(PermissionDenied)
		return
	}
	sourceIP, _ := remoteIP(in.Conn())
	s.mu.Lock()
	s.pruneLocked()
	reservation, ok := s.reservations[target]
	if !ok {
		s.mu.Unlock()
		fail(NoReservation)
		return
	}
	s.mu.Unlock()
	if !s.permitted(reservation.conn) {
		fail(PermissionDenied)
		return
	}
	s.mu.Lock()
	// Check the exact reserving connection, never dial supplied peer addresses.
	current, ok := s.reservations[target]
	if !ok || current.conn != reservation.conn || current.conn.Context().Err() != nil || !current.expires.After(time.Now()) {
		s.mu.Unlock()
		fail(NoReservation)
		return
	}
	ips := []netip.Addr{sourceIP}
	if reservation.ip != sourceIP {
		ips = append(ips, reservation.ip)
	}
	full := s.active >= s.cfg.MaxCircuits || s.byPeer[source] >= s.cfg.CircuitsPerPeer || s.byPeer[target] >= s.cfg.CircuitsPerPeer
	for _, ip := range ips {
		full = full || s.byIP[ip] >= s.cfg.CircuitsPerIP
	}
	if full {
		s.mu.Unlock()
		fail(ResourceLimit)
		return
	}
	s.active++
	s.byPeer[source]++
	s.byPeer[target]++
	for _, ip := range ips {
		s.byIP[ip]++
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		for _, id := range []wire.PeerID{source, target} {
			s.byPeer[id]--
			if s.byPeer[id] == 0 {
				delete(s.byPeer, id)
			}
		}
		for _, ip := range ips {
			s.byIP[ip]--
			if s.byIP[ip] == 0 {
				delete(s.byIP, ip)
			}
		}
		s.mu.Unlock()
	}()
	limit := s.cfg.Limited
	if s.admitted[source] && s.admitted[target] {
		limit = s.cfg.FullLimit
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()
	// Opening on this connection (not host lookup) prevents reservation reuse
	// across a disconnect/reconnect race.
	out, err := reservation.conn.OpenStream(ctx)
	if err != nil {
		fail(ConnectionFailed)
		return
	}
	defer out.Close()
	stopOut := context.AfterFunc(s.ctx, out.Reset)
	defer stopOut()
	deadline, _ := ctx.Deadline()
	_ = out.SetDeadline(deadline)
	if err = wire.Select(out, StopProtocol); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	if err = writeMessage(out, message{kind: 0, peer: source, limit: limit}, true); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	response, err := readMessage(out, true)
	if err != nil || response.kind != 1 || response.status != OK {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	if err = writeMessage(in, message{kind: 2, status: OK, limit: limit}, false); err != nil {
		in.Reset()
		out.Reset()
		return
	}
	releasePending()
	cancel()
	_ = in.SetDeadline(time.Time{})
	_ = out.SetDeadline(time.Time{})
	bridge(s.ctx, in, out, limit)
}

// bridge keeps FIN semantics in both directions. Duration expires from successful
// circuit setup; exceeding either directional byte budget resets BOTH streams.
func bridge(ctx context.Context, a, b *host.Stream, limit Limit) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if limit.Duration != 0 {
		var timeout context.CancelFunc
		ctx, timeout = context.WithTimeout(ctx, time.Duration(limit.Duration)*time.Second)
		defer timeout()
	}
	stop := context.AfterFunc(ctx, func() { a.Reset(); b.Reset() })
	defer stop()
	results := make(chan error, 2)
	copyOne := func(dst, src *host.Stream) {
		err := copyLimited(dst, src, limit.Data)
		if err == nil {
			err = dst.CloseWrite()
		}
		results <- err
	}
	go copyOne(a, b)
	go copyOne(b, a)
	if err := <-results; err != nil {
		a.Reset()
		b.Reset()
	}
	if err := <-results; err != nil {
		a.Reset()
		b.Reset()
	}
}
func copyLimited(dst io.Writer, src io.Reader, limit uint64) error {
	buf := make([]byte, 16<<10)
	var used uint64
	for {
		size := len(buf)
		if limit != 0 && limit-used < uint64(size) {
			size = int(limit-used) + 1
		}
		n, err := src.Read(buf[:size])
		allowed := n
		if limit != 0 && uint64(n) > limit-used {
			allowed = int(limit - used)
		}
		if allowed > 0 {
			written, e := dst.Write(buf[:allowed])
			if e != nil {
				return e
			}
			if written != allowed {
				return io.ErrShortWrite
			}
			used += uint64(written)
		}
		if allowed != n {
			return ResourceLimit
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
