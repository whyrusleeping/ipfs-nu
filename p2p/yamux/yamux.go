// Package yamux implements the bounded libp2p /yamux/1.0.0 stream multiplexer.
// Each stream has the standard fixed 256 KiB receive window. There is no window
// autotuning, speculative allocation, or unbounded accept/control queue.
package yamux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

const ID = "/yamux/1.0.0"
const Window = 256 << 10
const maxWrite = 16 << 10
const (
	dataType byte = iota
	windowType
	pingType
	awayType
)
const (
	syn uint16 = 1 << iota
	ack
	fin
	rst
)

var ErrProtocol = errors.New("yamux: protocol violation")
var ErrLimit = errors.New("yamux: resource limit")
var ErrReset = errors.New("yamux: stream reset")

type header struct {
	typ        byte
	flags      uint16
	id, length uint32
}

func decodeHeader(b []byte) (header, error) {
	var h header
	if len(b) != 12 || b[0] != 0 {
		return h, ErrProtocol
	}
	h = header{b[1], binary.BigEndian.Uint16(b[2:]), binary.BigEndian.Uint32(b[4:]), binary.BigEndian.Uint32(b[8:])}
	if h.typ > awayType || h.flags & ^uint16(15) != 0 {
		return h, ErrProtocol
	}
	if h.typ >= pingType {
		if h.id != 0 {
			return h, ErrProtocol
		}
		if h.typ == pingType && h.flags != syn && h.flags != ack {
			return h, ErrProtocol
		}
		if h.typ == awayType && (h.flags != 0 || h.length > 2) {
			return h, ErrProtocol
		}
	} else {
		if h.id == 0 || h.flags&syn != 0 && h.flags&ack != 0 || h.flags&rst != 0 && h.flags & ^rst != 0 {
			return h, ErrProtocol
		}
		if h.typ == dataType && h.length > Window {
			return h, ErrLimit
		}
	}
	return h, nil
}
func (h header) encode() []byte {
	b := make([]byte, 12)
	b[1] = h.typ
	binary.BigEndian.PutUint16(b[2:], h.flags)
	binary.BigEndian.PutUint32(b[4:], h.id)
	binary.BigEndian.PutUint32(b[8:], h.length)
	return b
}

type Config struct {
	MaxStreams   int           // default 64, includes streams waiting for AcceptStream
	WriteTimeout time.Duration // default 10s; a stuck TCP writer closes the session
}
type outbound struct {
	h    header
	data []byte
	done chan error
}

// Session owns its connection. Close interrupts and joins both I/O workers.
type Session struct {
	conn             net.Conn
	ctx              context.Context
	cancel           context.CancelFunc
	cfg              Config
	client           bool
	mu               sync.Mutex
	streams          map[uint32]*Stream
	next, lastRemote uint32
	err              error
	incoming         chan *Stream
	output           chan outbound
	wg               sync.WaitGroup
}

func New(conn net.Conn, client bool, cfg Config) (*Session, error) {
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 64
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.MaxStreams < 1 || cfg.MaxStreams > 1024 || cfg.WriteTimeout < 0 {
		conn.Close()
		return nil, ErrLimit
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{conn: conn, ctx: ctx, cancel: cancel, cfg: cfg, client: client, streams: make(map[uint32]*Stream), next: 2, incoming: make(chan *Stream, cfg.MaxStreams), output: make(chan outbound, 128)}
	if client {
		s.next = 1
	}
	s.wg.Add(2)
	go s.readLoop()
	go s.writeLoop()
	return s, nil
}
func (s *Session) Context() context.Context { return s.ctx }
func (s *Session) LocalAddr() net.Addr      { return s.conn.LocalAddr() }
func (s *Session) RemoteAddr() net.Addr     { return s.conn.RemoteAddr() }
func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		s.cancel()
		for _, st := range s.streams {
			st.signalLocked()
		}
	}
	s.mu.Unlock()
	s.conn.Close()
}
func (s *Session) Close() error { s.fail(net.ErrClosed); s.wg.Wait(); return nil }
func (s *Session) newStreamLocked(id uint32) *Stream {
	st := &Stream{session: s, id: id, recvWindow: Window, sendWindow: Window, changed: make(chan struct{})}
	s.streams[id] = st
	return st
}
func (s *Session) OpenStream(ctx context.Context) (*Stream, error) {
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	if len(s.streams) >= s.cfg.MaxStreams || s.next > math.MaxUint32-2 {
		s.mu.Unlock()
		return nil, ErrLimit
	}
	st := s.newStreamLocked(s.next)
	s.next += 2
	s.mu.Unlock()
	if err := s.send(ctx, outbound{h: header{typ: windowType, flags: syn, id: st.id}}, time.Time{}); err != nil {
		st.Reset()
		return nil, err
	}
	return st, nil
}
func (s *Session) AcceptStream(ctx context.Context) (*Stream, error) {
	for {
		select {
		case st := <-s.incoming:
			s.mu.Lock()
			err := st.err
			s.mu.Unlock()
			if err != nil {
				continue
			}
			return st, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, net.ErrClosed
		}
	}
}

// control never blocks the reader on the writer: a peer filling the bounded
// control queue without reading responses loses the whole session.
func (s *Session) control(h header) error {
	select {
	case s.output <- outbound{h: h}:
		return nil
	case <-s.ctx.Done():
		return net.ErrClosed
	default:
		return ErrLimit
	}
}
func (s *Session) send(ctx context.Context, o outbound, deadline time.Time) error {
	var timer *time.Timer
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		timeout = timer.C
		defer timer.Stop()
	}
	o.done = make(chan error, 1)
	select {
	case s.output <- o:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return net.ErrClosed
	case <-timeout:
		return os.ErrDeadlineExceeded
	}
	select {
	case err := <-o.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return net.ErrClosed
	case <-timeout:
		return os.ErrDeadlineExceeded
	}
}
func (s *Session) writeLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case o := <-s.output:
			b := append(o.h.encode(), o.data...)
			err := s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
			for err == nil && len(b) > 0 {
				var n int
				n, err = s.conn.Write(b)
				if n <= 0 && err == nil {
					err = io.ErrShortWrite
				}
				b = b[n:]
			}
			if o.done != nil {
				o.done <- err
			}
			if err != nil {
				s.fail(err)
				return
			}
		}
	}
}
func (s *Session) readLoop() {
	defer s.wg.Done()
	for {
		var b [12]byte
		if _, err := io.ReadFull(s.conn, b[:]); err != nil {
			s.fail(err)
			return
		}
		h, err := decodeHeader(b[:])
		if err == nil {
			err = s.receive(h)
		}
		if err != nil {
			s.fail(err)
			return
		}
	}
}
func (s *Session) receive(h header) error {
	if h.typ == pingType {
		if h.flags == syn {
			return s.control(header{typ: pingType, flags: ack, length: h.length})
		}
		return nil
	}
	if h.typ == awayType {
		return fmt.Errorf("yamux: remote go-away code %d", h.length)
	}
	s.mu.Lock()
	st := s.streams[h.id]
	if h.flags&syn != 0 {
		remoteOdd := !s.client
		if st != nil || h.id <= s.lastRemote || (h.id%2 == 1) != remoteOdd {
			s.mu.Unlock()
			return ErrProtocol
		}
		s.lastRemote = h.id
		if len(s.streams) < s.cfg.MaxStreams {
			st = s.newStreamLocked(h.id)
			st.acknowledged = true
		}
	}
	if st == nil {
		s.mu.Unlock()
		if h.typ == dataType {
			if _, err := io.CopyN(io.Discard, s.conn, int64(h.length)); err != nil {
				return err
			}
		}
		if h.flags&rst == 0 {
			return s.control(header{typ: windowType, flags: rst, id: h.id})
		}
		return nil
	}
	if h.flags&ack != 0 {
		if st.acknowledged {
			s.mu.Unlock()
			return ErrProtocol
		}
		st.acknowledged = true
	}
	if h.flags&rst != 0 {
		st.err = ErrReset
		st.buf.Reset()
		st.signalLocked()
		delete(s.streams, h.id)
		s.mu.Unlock()
		if h.typ == dataType {
			_, err := io.CopyN(io.Discard, s.conn, int64(h.length))
			return err
		}
		return nil
	}
	if h.typ == windowType {
		if uint64(st.sendWindow)+uint64(h.length) > math.MaxUint32 {
			s.mu.Unlock()
			return ErrProtocol
		}
		st.sendWindow += h.length
	} else {
		if st.remoteFIN || h.length > st.recvWindow {
			s.mu.Unlock()
			return ErrProtocol
		}
		st.recvWindow -= h.length
	}
	s.mu.Unlock()
	var payload []byte
	if h.typ == dataType && h.length > 0 {
		payload = make([]byte, int(h.length))
		if _, err := io.ReadFull(s.conn, payload); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if st.err == nil {
		st.buf.Write(payload)
		if h.flags&fin != 0 {
			st.remoteFIN = true
		}
		st.signalLocked()
	}
	s.mu.Unlock()
	if h.flags&syn != 0 {
		if err := s.control(header{typ: windowType, flags: ack, id: h.id}); err != nil {
			return err
		}
		select {
		case s.incoming <- st:
		default:
			st.Reset()
		}
	}
	return nil
}

// Stream deadlines are independent of other streams. A write timeout resets the
// stream because bytes may already be queued on the reliable transport. Close
// sends FIN and releases local resources; CloseWrite permits subsequent reads.
type Stream struct {
	session                           *Session
	id                                uint32
	readMu, writeMu                   sync.Mutex
	buf                               bytes.Buffer
	recvWindow, sendWindow, consumed  uint32
	localFIN, remoteFIN, acknowledged bool
	err                               error
	readDeadline, writeDeadline       time.Time
	changed                           chan struct{} // all state above (except I/O mutexes) under session.mu
}

func (st *Stream) signalLocked() { close(st.changed); st.changed = make(chan struct{}) }
func wait(ctx context.Context, changed <-chan struct{}, deadline time.Time) error {
	var timer *time.Timer
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		timeout = timer.C
		defer timer.Stop()
	}
	select {
	case <-changed:
		return nil
	case <-ctx.Done():
		return net.ErrClosed
	case <-timeout:
		return os.ErrDeadlineExceeded
	}
}
func (st *Stream) Read(b []byte) (int, error) {
	st.readMu.Lock()
	defer st.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	s := st.session
	for {
		s.mu.Lock()
		if st.err != nil {
			err := st.err
			s.mu.Unlock()
			return 0, err
		}
		if !st.readDeadline.IsZero() && !time.Now().Before(st.readDeadline) {
			s.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if st.buf.Len() > 0 {
			n, _ := st.buf.Read(b)
			st.consumed += uint32(n)
			var credit uint32
			if st.consumed >= Window/2 {
				credit = st.consumed
				st.consumed = 0
				st.recvWindow += credit
			}
			s.mu.Unlock()
			if credit != 0 {
				if err := s.control(header{typ: windowType, id: st.id, length: credit}); err != nil {
					s.fail(err)
					return n, err
				}
			}
			return n, nil
		}
		if st.remoteFIN {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return 0, err
		}
		changed, deadline := st.changed, st.readDeadline
		s.mu.Unlock()
		if err := wait(s.ctx, changed, deadline); err != nil {
			return 0, err
		}
	}
}
func (st *Stream) Write(b []byte) (int, error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.session
	total := 0
	for len(b) > 0 {
		s.mu.Lock()
		err := st.err
		if err == nil {
			err = s.err
		}
		if err == nil && st.localFIN {
			err = net.ErrClosed
		}
		if err != nil {
			s.mu.Unlock()
			return total, err
		}
		changed, deadline := st.changed, st.writeDeadline
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			s.mu.Unlock()
			return total, os.ErrDeadlineExceeded
		}
		if st.sendWindow == 0 {
			s.mu.Unlock()
			if err := wait(s.ctx, changed, deadline); err != nil {
				return total, err
			}
			continue
		}
		n := min(len(b), maxWrite, int(st.sendWindow))
		st.sendWindow -= uint32(n)
		s.mu.Unlock()
		o := outbound{h: header{typ: dataType, id: st.id, length: uint32(n)}, data: bytes.Clone(b[:n])}
		if err := s.send(s.ctx, o, deadline); err != nil {
			st.Reset()
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}
func (st *Stream) CloseWrite() error {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	s := st.session
	s.mu.Lock()
	if st.localFIN || st.err != nil {
		s.mu.Unlock()
		return nil
	}
	st.localFIN = true
	deadline := st.writeDeadline
	s.mu.Unlock()
	err := s.send(s.ctx, outbound{h: header{typ: windowType, flags: fin, id: st.id}}, deadline)
	if err != nil {
		st.Reset()
	}
	return err
}
func (st *Stream) Close() error {
	err := st.CloseWrite()
	s := st.session
	s.mu.Lock()
	delete(s.streams, st.id)
	st.err = net.ErrClosed
	st.buf.Reset()
	st.signalLocked()
	s.mu.Unlock()
	return err
}
func (st *Stream) Reset() {
	s := st.session
	s.mu.Lock()
	if st.err != nil {
		s.mu.Unlock()
		return
	}
	st.err = ErrReset
	st.buf.Reset()
	delete(s.streams, st.id)
	st.signalLocked()
	s.mu.Unlock()
	if err := s.control(header{typ: windowType, flags: rst, id: st.id}); err != nil {
		s.fail(err)
	}
}
func (st *Stream) LocalAddr() net.Addr  { return st.session.LocalAddr() }
func (st *Stream) RemoteAddr() net.Addr { return st.session.RemoteAddr() }
func (st *Stream) SetDeadline(t time.Time) error {
	s := st.session
	s.mu.Lock()
	st.readDeadline = t
	st.writeDeadline = t
	st.signalLocked()
	s.mu.Unlock()
	return nil
}
func (st *Stream) SetReadDeadline(t time.Time) error {
	s := st.session
	s.mu.Lock()
	st.readDeadline = t
	st.signalLocked()
	s.mu.Unlock()
	return nil
}
func (st *Stream) SetWriteDeadline(t time.Time) error {
	s := st.session
	s.mu.Lock()
	st.writeDeadline = t
	st.signalLocked()
	s.mu.Unlock()
	return nil
}
