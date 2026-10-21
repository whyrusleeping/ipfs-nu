package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// ServerConfig bounds the data protocol servers. Zero values select the defaults.
type ServerConfig struct {
	// MaxRequests is the concurrent request limit per protocol (default 64).
	MaxRequests int
	// MaxPeerRequests is the concurrent request limit per peer and protocol (default 16).
	MaxPeerRequests int
	// RequestTimeout bounds reading the request (default 10s).
	RequestTimeout time.Duration
	// StallTimeout resets a response the peer stops reading for this long (default 30s).
	StallTimeout time.Duration
	// TreeFrames and TreeBytes are the /nu/tree/1 response budget: data frames and their
	// bytes (DefaultTreeFrames, DefaultTreeBytes). A response always carries one frame.
	TreeFrames int
	TreeBytes  int
}

// Server answers /nu/bytes/1, /nu/have/1 and /nu/tree/1 from a store.
type Server struct {
	st     *store.Store
	cfg    ServerConfig
	mu     sync.Mutex
	active map[string]int // protocol -> requests
	peers  map[peerProto]int
}

type peerProto struct {
	peer  wire.PeerID
	proto string
}

// serverStream is the part of *host.Stream a handler uses.
type serverStream interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Reset()
}

// NewServer returns a server for st.
func NewServer(st *store.Store, cfg ServerConfig) *Server {
	if cfg.MaxRequests <= 0 {
		cfg.MaxRequests = 64
	}
	if cfg.MaxPeerRequests <= 0 {
		cfg.MaxPeerRequests = 16
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 30 * time.Second
	}
	if cfg.TreeFrames <= 0 {
		cfg.TreeFrames = DefaultTreeFrames
	}
	if cfg.TreeBytes <= 0 {
		cfg.TreeBytes = DefaultTreeBytes
	}
	return &Server{st: st, cfg: cfg, active: make(map[string]int), peers: make(map[peerProto]int)}
}

// Register installs the protocol handlers on h. The host's own per-protocol limit is set
// above MaxRequests so that excess requests get a cheap busy status instead of a refused
// protocol.
func (s *Server) Register(h *host.Host) error {
	if err := h.Handle(BytesID, 2*s.cfg.MaxRequests, func(st *host.Stream) { s.serve(BytesID, st.Conn().PeerID(), st, s.serveBytes) }); err != nil {
		return err
	}
	if err := h.Handle(HaveID, 2*s.cfg.MaxRequests, func(st *host.Stream) { s.serve(HaveID, st.Conn().PeerID(), st, s.serveHave) }); err != nil {
		return err
	}
	return h.Handle(TreeID, 2*s.cfg.MaxRequests, func(st *host.Stream) { s.serve(TreeID, st.Conn().PeerID(), st, s.serveTree) })
}

// Active reports the number of requests being served.
func (s *Server) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.active {
		n += v
	}
	return n
}

func (s *Server) acquire(proto string, peer wire.PeerID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := peerProto{peer, proto}
	if s.active[proto] >= s.cfg.MaxRequests || s.peers[k] >= s.cfg.MaxPeerRequests {
		return false
	}
	s.active[proto]++
	s.peers[k]++
	return true
}

func (s *Server) release(proto string, peer wire.PeerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := peerProto{peer, proto}
	s.active[proto]--
	if s.peers[k]--; s.peers[k] == 0 {
		delete(s.peers, k)
	}
}

// failure is a refusal sent as a response status before any body.
type failure struct {
	status Status
	msg    string
}

func fail(st Status, msg string) *failure { return &failure{st, msg} }

// serve reads the request and runs fn. fn returns a refusal to send, or writes its own
// response; a write error resets the stream. The host dispatcher closes the stream (FIN).
func (s *Server) serve(proto string, peer wire.PeerID, st serverStream, fn func(req []byte, w *bufio.Writer) (*failure, error)) {
	w := bufio.NewWriterSize(stallWriter{st, s.cfg.StallTimeout}, 64<<10)
	if !s.acquire(proto, peer) {
		s.refuse(st, w, fail(StatusBusy, "too many concurrent requests"))
		return
	}
	defer s.release(proto, peer)
	_ = st.SetReadDeadline(time.Now().Add(s.cfg.RequestTimeout))
	req, err := wire.ReadMessage(st, MaxRequestSize)
	if errors.Is(err, wire.ErrLimit) {
		s.refuse(st, w, fail(StatusBadRequest, "request too large"))
		return
	}
	if err != nil {
		st.Reset()
		return
	}
	f, err := fn(req, w)
	if f != nil {
		if err != nil {
			slog.Warn("proto: request failed", "protocol", proto, "peer", peer, "err", err)
		}
		s.refuse(st, w, f)
		return
	}
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		slog.Debug("proto: response aborted", "protocol", proto, "peer", peer, "err", err)
		st.Reset()
	}
}

func (s *Server) refuse(st serverStream, w *bufio.Writer, f *failure) {
	b, err := drisl.Marshal(bytesHeader{Status: uint64(f.status), Error: f.msg})
	if err == nil {
		err = wire.WriteMessage(w, b, MaxBytesHeaderSize)
	}
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		st.Reset()
	}
}

// stallWriter resets the write deadline before each write, so a peer that stops reading
// frees the request after the stall timeout.
type stallWriter struct {
	st serverStream
	d  time.Duration
}

func (w stallWriter) Write(b []byte) (int, error) {
	_ = w.st.SetWriteDeadline(time.Now().Add(w.d))
	return w.st.Write(b)
}

func writeOK(w io.Writer, size uint64) error {
	b, err := drisl.Marshal(bytesHeader{Status: uint64(StatusOK), Size: &size})
	if err != nil {
		return err
	}
	return wire.WriteMessage(w, b, MaxBytesHeaderSize)
}

func (s *Server) serveBytes(b []byte, w *bufio.Writer) (*failure, error) {
	req, err := ParseBytesRequest(b)
	if err != nil {
		return fail(StatusBadRequest, err.Error()), nil
	}
	info, ok, err := s.st.Has(cid.New(cid.Raw, req.MH))
	if err != nil {
		return fail(StatusInternal, "storage error"), err
	}
	if !ok {
		return fail(StatusNotFound, "object not held"), nil
	}
	if !info.Complete {
		f, err := s.servePartial(req, w)
		if !errors.Is(err, store.ErrComplete) {
			return f, err
		}
	}
	bl, err := s.st.OpenBlob(req.MH)
	if errors.Is(err, store.ErrNotFound) {
		return fail(StatusNotFound, "object not held"), nil
	} else if err != nil {
		return fail(StatusInternal, "storage error"), err
	}
	defer bl.Close()
	if req.MH.Hash == cid.SHA256 {
		if err := writeOK(w, bl.Size); err != nil {
			return nil, err
		}
		_, err := io.Copy(w, bl.Reader())
		return nil, err
	}
	ranges, f := wholeOr(req.Ranges, bl.Size)
	if f != nil {
		return f, nil
	}
	ob := bl.Outboard()
	if ob == nil {
		// Inline blobs and BLAKE3 DRISL blocks keep no outboard; build it in memory (they
		// are at most 8 MiB, so the outboard is at most 32 KiB).
		buf := make(memAt, blob.OutboardSize(bl.Size))
		if blob.HasOutboard(bl.Size) {
			if _, err := blob.Outboard(buf, bl.Reader(), bl.Size); err != nil {
				return fail(StatusInternal, "storage error"), err
			}
		} else {
			binary.LittleEndian.PutUint64(buf, bl.Size)
		}
		ob = buf
	}
	if err := writeOK(w, bl.Size); err != nil {
		return nil, err
	}
	for _, r := range ranges {
		if err := blob.ExtractSlice(w, bl, ob, bl.Size, r); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s *Server) servePartial(req BytesRequest, w *bufio.Writer) (*failure, error) {
	p, err := s.st.OpenPartial(req.MH)
	if errors.Is(err, store.ErrNotFound) {
		return fail(StatusNotFound, "object not held"), nil
	} else if err != nil {
		return nil, err
	}
	defer p.Close()
	if len(req.Ranges) == 0 {
		return fail(StatusNotHeld, "blob held partially; request held ranges"), nil
	}
	ranges, f := wholeOr(req.Ranges, p.Size)
	if f != nil {
		return f, nil
	}
	for _, r := range ranges {
		if !p.Has(r) {
			return fail(StatusNotHeld, "requested ranges not held"), nil
		}
	}
	if err := writeOK(w, p.Size); err != nil {
		return nil, err
	}
	for _, r := range ranges {
		if err := p.ExtractSlice(w, r); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// wholeOr validates requested ranges against size; none means the whole blob.
func wholeOr(rs []blob.Range, size uint64) ([]blob.Range, *failure) {
	if len(rs) == 0 {
		return []blob.Range{{Start: 0, End: blob.Groups(size)}}, nil
	}
	for _, r := range rs {
		if !r.Valid(size) {
			return nil, fail(StatusBadRequest, "range outside the blob")
		}
	}
	return rs, nil
}

func (s *Server) serveHave(b []byte, w *bufio.Writer) (*failure, error) {
	qs, err := ParseHaveRequest(b)
	if err != nil {
		return fail(StatusBadRequest, err.Error()), nil
	}
	resp := haveResponseWire{Have: make([]byte, (len(qs)+7)/8)}
	for i, q := range qs {
		res, err := s.have(q)
		if err != nil {
			return fail(StatusInternal, "storage error"), err
		}
		if !res.Held {
			continue
		}
		resp.Have[i/8] |= 1 << (i % 8)
		resp.Info = append(resp.Info, haveAnswer(res))
	}
	out, err := drisl.Marshal(resp)
	if err != nil {
		return fail(StatusInternal, "response too large"), err
	}
	return nil, wire.WriteMessage(w, out, MaxHaveResponseSize)
}

func (s *Server) have(q HaveQuery) (HaveResult, error) {
	info, ok, err := s.st.Has(cid.New(cid.Raw, q.MH))
	if err != nil || !ok {
		return HaveResult{}, err
	}
	res := HaveResult{Held: true, Codec: info.Codec, Size: info.Size, Complete: info.Complete}
	if info.Complete {
		return res, nil
	}
	_, held, complete, err := s.st.HeldRanges(q.MH)
	if err != nil {
		return HaveResult{}, err
	}
	if complete {
		res.Complete = true
		return res, nil
	}
	groups := blob.Groups(info.Size)
	win := blob.Range{Start: 0, End: groups}
	if q.Window != nil {
		win = *q.Window
	}
	win.Start = min(win.Start, groups)
	win.End = max(win.Start, min(win.End, groups, win.Start+MaxHaveWindow))
	res.Window = win
	for _, r := range held {
		r.Start, r.End = max(r.Start, win.Start), min(r.End, win.End)
		if r.Start < r.End {
			res.Ranges = append(res.Ranges, r)
		}
	}
	return res, nil
}

// memAt is an in-memory io.ReaderAt/io.WriterAt of fixed size.
type memAt []byte

func (m memAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m)) {
		return 0, io.EOF
	}
	n := copy(p, m[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m memAt) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m)) {
		return 0, errors.New("proto: write outside buffer")
	}
	return copy(m[off:], p), nil
}
