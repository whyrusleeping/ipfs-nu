package autonat

import (
	"context"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type entry struct {
	peer wire.PeerID
	at   time.Time
}

// All retained rate-limit state is bounded by the global minute limit, even
// under a flood of identities. Rejections do not create per-peer entries.
type limiter struct {
	mu     sync.Mutex
	recent []entry
	data   []time.Time
	active map[wire.PeerID]bool
}

func (l *limiter) admit(id wire.PeerID, now time.Time, cfg Config) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := 0
	for first < len(l.recent) && now.Sub(l.recent[first].at) >= time.Minute {
		first++
	}
	l.recent = append(l.recent[:0], l.recent[first:]...)
	count := 0
	for _, e := range l.recent {
		if e.peer == id {
			count++
		}
	}
	if l.active[id] || len(l.recent) >= cfg.RequestsPerMinute || count >= cfg.PerPeerPerMinute {
		return false
	}
	l.recent = append(l.recent, entry{id, now})
	l.active[id] = true
	return true
}
func (l *limiter) release(id wire.PeerID) {
	l.mu.Lock()
	delete(l.active, id)
	l.mu.Unlock()
}
func (l *limiter) admitData(now time.Time, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := 0
	for first < len(l.data) && now.Sub(l.data[first]) >= time.Minute {
		first++
	}
	l.data = append(l.data[:0], l.data[first:]...)
	if len(l.data) >= max {
		return false
	}
	l.data = append(l.data, now)
	return true
}
func (s *Service) handleRequest(stream *host.Stream) {
	if !s.begin() {
		stream.Reset()
		return
	}
	defer s.wg.Done()
	ctx, cleanup := s.bind(stream.Conn().Context(), stream)
	defer cleanup()
	respond := func(status, dialStatus uint32, index uint32) {
		if err := writeMessage(stream, message{kind: dialResponse, status: status, dialStatus: dialStatus, index: index}); err != nil {
			stream.Reset()
		}
	}
	peer := stream.Conn().PeerID()
	if !s.lim.admit(peer, time.Now(), s.cfg) {
		respond(responseRejected, 0, 0)
		return
	}
	defer s.lim.release(peer)
	m, err := readMessage(stream)
	if err != nil || m.kind != dialRequest {
		stream.Reset()
		return
	}
	var target wire.Multiaddr
	var ip netip.Addr
	var index uint32
	for i, b := range m.addrs {
		a, err := wire.MultiaddrFromBytes(b, 1024)
		if err != nil {
			continue
		}
		a, candidateIP, err := endpoint(a, peer, s.cfg.AllowPrivateAddresses)
		if err != nil {
			continue
		}
		target, ip, index = a, candidateIP, uint32(i)
		break
	}
	if !ip.IsValid() {
		respond(responseRefused, 0, 0)
		return
	}
	observed, ok := stream.Conn().RemoteAddr().(*net.UDPAddr)
	if !ok {
		respond(responseInternal, 0, 0)
		return
	}
	if ip != observed.AddrPort().Addr().Unmap() {
		if !s.lim.admitData(time.Now(), s.cfg.DialDataPerMinute) {
			respond(responseRejected, 0, 0)
			return
		}
		// Charge 30–100 kB before causing any handshake to a different IP.
		n := minDialData + rand.IntN(maxDialData-minDialData+1)
		if err := receiveData(stream, index, n); err != nil {
			stream.Reset()
			return
		}
	}
	if ctx.Err() != nil {
		stream.Reset()
		return
	}
	status, err := s.probe(ctx, target, ip, m.nonce)
	if err != nil {
		respond(responseInternal, 0, 0)
		return
	}
	respond(responseOK, status, index)
}
func receiveData(stream *host.Stream, index uint32, n int) error {
	if err := writeMessage(stream, message{kind: dataRequest, index: index, numBytes: uint64(n)}); err != nil {
		return err
	}
	for n > 0 {
		m, err := readMessage(stream)
		if err != nil {
			return err
		}
		if m.kind != dataResponse || len(m.data) == 0 || (len(m.data) < 100 && len(m.data) < n) {
			return wire.ErrFormat
		}
		n -= len(m.data) // only the protobuf data payload counts, never frame overhead
	}
	return nil
}

// A separate host per probe guarantees a fresh source port/identity and exactly
// one destination. Reusing the request connection or listen port can accidentally
// open a NAT mapping and falsely classify reachability. Probe hosts are bounded
// by the handler concurrency limit and always closed before the final response.
func (s *Service) probe(ctx context.Context, target wire.Multiaddr, ip netip.Addr, nonce uint64) (uint32, error) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return 0, err
	}
	listen := "0.0.0.0:0"
	if ip.Is6() {
		listen = "[::]:0"
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{listen}, Limits: host.Limits{MaxConnections: 1, MaxStreams: 2}})
	if err != nil {
		return 0, err
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	conn, err := h.Dial(ctx, target)
	if err != nil {
		return dialError, nil
	}
	stream, err := h.OpenStream(ctx, conn.PeerID(), DialBackProtocol)
	if err != nil {
		return dialBackError, nil
	}
	defer stream.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, stream.Reset)
	defer stop()
	if err := wire.WriteMessage(stream, fixed64(nil, 1, nonce), 1024); err != nil {
		stream.Reset()
		return dialBackError, nil
	}
	_ = stream.CloseWrite()
	// Wait for receipt/closure before tearing down the probe host. Deployed
	// go-libp2p clients Close then Reset this stream, sometimes discarding their
	// queued acknowledgement. Like go-libp2p's server, a read error here does not
	// undo a successfully sent nonce: DialStatus describes sending, and only
	// the client's nonce receipt can establish reachability.
	b, err := wire.ReadMessage(stream, 1024)
	if err == nil && parseDialBackResponse(b) != nil {
		stream.Reset()
		return dialBackError, nil
	}
	return dialOK, nil
}
