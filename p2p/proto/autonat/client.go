package autonat

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Check tests the first address the server is willing to dial, preserving request
// priority. Connect to server first (e.g. via host.Sessions). It does not discover
// servers, schedule rechecks, aggregate observations, or publish addresses.
// A dial failure yields Unreachable for this observer, not proof about other paths.
func (s *Service) Check(ctx context.Context, server wire.PeerID, reqs []Request) (Result, error) {
	if !s.begin() {
		return Result{}, net.ErrClosed
	}
	defer s.wg.Done()
	select {
	case s.requests <- struct{}{}:
		defer func() { <-s.requests }()
	default:
		return Result{}, host.ErrLimit
	}
	if len(reqs) == 0 || len(reqs) > maxAddresses {
		return Result{}, wire.ErrLimit
	}
	m := message{kind: dialRequest}
	for _, req := range reqs {
		if _, _, err := endpoint(req.Address, s.host.ID(), s.cfg.AllowPrivateAddresses); err != nil {
			return Result{}, err
		}
		m.addrs = append(m.addrs, req.Address.Bytes())
	}
	if len(m.marshal()) > maxMessage {
		return Result{}, wire.ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	stopService := context.AfterFunc(s.ctx, cancel)
	defer stopService()
	stream, err := s.host.OpenStreamLazy(ctx, server, DialProtocol)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()
	ctx, cleanup := s.bind(ctx, stream)
	defer cleanup()
	p := pending{conn: stream.Conn(), ch: make(chan receipt, 1)}
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Result{}, err
		}
		m.nonce = binary.LittleEndian.Uint64(b[:])
		s.mu.Lock()
		_, exists := s.pending[m.nonce]
		if !exists {
			s.pending[m.nonce] = p
		}
		s.mu.Unlock()
		if !exists {
			break
		}
	}
	defer func() { s.mu.Lock(); delete(s.pending, m.nonce); s.mu.Unlock() }()
	result, err := s.exchange(ctx, stream, m, reqs, p.ch)
	if err != nil {
		stream.Reset()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	return result, err
}
func (s *Service) exchange(ctx context.Context, stream *host.Stream, request message, reqs []Request, ch <-chan receipt) (Result, error) {
	if err := writeMessage(stream, request); err != nil {
		return Result{}, err
	}
	m, err := readMessage(stream)
	if err != nil {
		return Result{}, err
	}
	var charged *uint32
	if m.kind == dataRequest {
		if uint64(m.index) >= uint64(len(reqs)) || m.numBytes == 0 || m.numBytes > maxDialData || !reqs[m.index].SendDialData {
			return Result{}, ErrDialData
		}
		index := m.index
		charged = &index
		data := make([]byte, maxData)
		for n := m.numBytes; n > 0; {
			count := min(uint64(len(data)), n)
			if err := writeMessage(stream, message{kind: dataResponse, data: data[:count]}); err != nil {
				return Result{}, err
			}
			n -= count
		}
		m, err = readMessage(stream)
		if err != nil {
			return Result{}, err
		}
	}
	if m.kind != dialResponse {
		return Result{}, wire.ErrFormat
	}
	switch m.status {
	case responseRefused:
		return Result{}, ErrRefused
	case responseRejected:
		return Result{}, ErrRejected
	case responseOK:
	default:
		return Result{}, fmt.Errorf("autonat: unsuccessful or unknown response status %d", m.status)
	}
	if uint64(m.index) >= uint64(len(reqs)) || (charged != nil && *charged != m.index) {
		return Result{}, wire.ErrFormat
	}
	r := Result{Address: reqs[m.index].Address, Index: int(m.index), Server: stream.Conn().PeerID(), Endpoint: stream.Conn().RemoteAddr().String(), Checked: time.Now()}
	switch m.dialStatus {
	case dialError:
		r.Reachability = Unreachable
		return r, nil
	case dialOK, dialBackError:
		// The response can race the dial-back handler. Give its independent
		// stream a short window, still bounded by caller/service cancellation.
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case got := <-ch:
			local, ok := got.local.(*net.UDPAddr)
			_, ip, err := endpoint(r.Address, s.host.ID(), s.cfg.AllowPrivateAddresses)
			if !ok || err != nil || local.AddrPort().Addr().Unmap().Is6() != ip.Is6() {
				return r, ErrUnverified
			}
			r.Reachability = Reachable
			return r, nil
		case <-ctx.Done():
			return r, ctx.Err()
		case <-timer.C:
			return r, ErrUnverified
		}
	default:
		return r, fmt.Errorf("autonat: unknown dial status %d", m.dialStatus)
	}
}
func (s *Service) handleDialBack(stream *host.Stream) {
	if !s.begin() {
		stream.Reset()
		return
	}
	defer s.wg.Done()
	_, cleanup := s.bind(stream.Conn().Context(), stream)
	defer cleanup()
	_ = stream.SetDeadline(time.Now().Add(min(s.cfg.Timeout, 5*time.Second)))
	b, err := wire.ReadMessage(stream, 1024)
	if err != nil {
		stream.Reset()
		return
	}
	nonce, err := parseDialBack(b)
	if err != nil {
		stream.Reset()
		return
	}
	s.mu.Lock()
	p, ok := s.pending[nonce]
	if ok && p.conn != stream.Conn() {
		// Consume once: unknown, duplicate and request-connection dial backs
		// cannot become reachability evidence. Server identity is irrelevant.
		delete(s.pending, nonce)
		p.ch <- receipt{local: stream.Conn().LocalAddr()}
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		stream.Reset()
		return
	}
	if err := wire.WriteMessage(stream, nil, 1024); err != nil {
		stream.Reset()
		return
	}
	_ = stream.CloseWrite()
}
