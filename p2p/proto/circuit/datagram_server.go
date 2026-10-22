package circuit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// channelAlloc hands out per-connection datagram channel IDs. The relay is the
// single allocator on each of its connections, so IDs never collide.
type channelAlloc struct {
	next uint16
	used map[uint16]bool
}

func (a *channelAlloc) take() (uint16, bool) {
	if len(a.used) >= 64 {
		return 0, false
	}
	for range 65536 {
		a.next++
		if a.next != 0 && !a.used[a.next] {
			a.used[a.next] = true
			return a.next, true
		}
	}
	return 0, false
}

// relayLeg is one datagram pair on the relay: two control streams, two channels
// and two forwarders. Ending either control stream, either outer connection, the
// pair's last stream circuit or the relay ends the whole leg.
type relayLeg struct {
	pair   [2]wire.PeerID
	cancel context.CancelFunc
}

func (s *Server) datagramLimits() (max int) {
	return s.cfg.MaxCircuits
}
func (s *Server) allocChannel(conn *host.Conn) (uint16, bool) {
	if s.channels == nil {
		s.channels = make(map[*host.Conn]*channelAlloc)
	}
	a := s.channels[conn]
	if a == nil {
		a = &channelAlloc{used: make(map[uint16]bool)}
		s.channels[conn] = a
	}
	return a.take()
}
func (s *Server) releaseChannel(conn *host.Conn, id uint16) {
	if a := s.channels[conn]; a != nil {
		delete(a.used, id)
		if len(a.used) == 0 {
			delete(s.channels, conn)
		}
	}
}

// pairOpened/pairClosed track live stream circuits per peer pair; the last
// circuit closing also ends that pair's datagram legs (relay admission is bound
// to a consented circuit, never to a bare request).
func (s *Server) pairOpened(a, b wire.PeerID) {
	s.mu.Lock()
	if s.pairs == nil {
		s.pairs = make(map[[2]wire.PeerID]int)
	}
	s.pairs[pairKey(a, b)]++
	s.mu.Unlock()
}
func (s *Server) pairClosed(a, b wire.PeerID) {
	key := pairKey(a, b)
	s.mu.Lock()
	s.pairs[key]--
	var legs []*relayLeg
	if s.pairs[key] <= 0 {
		delete(s.pairs, key)
		for leg := range s.legs {
			if leg.pair == key {
				legs = append(legs, leg)
			}
		}
	}
	s.mu.Unlock()
	for _, leg := range legs {
		leg.cancel()
	}
}

// DatagramLegs reports the live datagram pairs (diagnostics).
func (s *Server) DatagramLegs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.legs)
}

func (s *Server) handleDatagram(in *host.Stream) {
	if !s.begin() {
		in.Reset()
		return
	}
	defer s.wg.Done()
	stopService := context.AfterFunc(s.ctx, in.Reset)
	defer stopService()
	_ = in.SetDeadline(time.Now().Add(s.cfg.Timeout))
	fail := func(status Status) { _ = writeDatagramMessage(in, datagramMessage{kind: dgFail, status: status}) }
	m, err := readDatagramMessage(in)
	if err != nil || m.kind != dgRequest || m.channel != 0 {
		fail(MalformedMessage)
		return
	}
	source, target := in.Conn().PeerID(), m.peer
	// Nu-only, full-service pairs: both admitted, both outer legs datagram-capable,
	// and a live consented stream circuit between exactly these peers.
	if !s.admitted[source] || !s.admitted[target] || source == target || !in.Conn().SupportsDatagrams() || !s.permitted(in.Conn()) {
		fail(PermissionDenied)
		return
	}
	s.mu.Lock()
	s.pruneLocked()
	reservation, ok := s.reservations[target]
	live := s.pairs[pairKey(source, target)] > 0
	full := len(s.legs) >= s.datagramLimits() || s.legsByPeer[source] >= s.cfg.CircuitsPerPeer || s.legsByPeer[target] >= s.cfg.CircuitsPerPeer
	s.mu.Unlock()
	if !ok || !live || reservation.conn.Context().Err() != nil || !reservation.expires.After(time.Now()) {
		fail(NoReservation)
		return
	}
	if !reservation.conn.SupportsDatagrams() {
		fail(PermissionDenied)
		return
	}
	if full {
		fail(ResourceLimit)
		return
	}
	legCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	leg := &relayLeg{pair: pairKey(source, target), cancel: cancel}
	s.mu.Lock()
	if len(s.legs) >= s.datagramLimits() {
		s.mu.Unlock()
		fail(ResourceLimit)
		return
	}
	if s.legs == nil {
		s.legs = make(map[*relayLeg]struct{})
		s.legsByPeer = make(map[wire.PeerID]int)
	}
	s.legs[leg] = struct{}{}
	s.legsByPeer[source]++
	s.legsByPeer[target]++
	sourceChannel, ok1 := s.allocChannel(in.Conn())
	targetChannel, ok2 := s.allocChannel(reservation.conn)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.legs, leg)
		for _, id := range []wire.PeerID{source, target} {
			s.legsByPeer[id]--
			if s.legsByPeer[id] <= 0 {
				delete(s.legsByPeer, id)
			}
		}
		if ok1 {
			s.releaseChannel(in.Conn(), sourceChannel)
		}
		if ok2 {
			s.releaseChannel(reservation.conn, targetChannel)
		}
		s.mu.Unlock()
	}()
	if !ok1 || !ok2 {
		fail(ResourceLimit)
		return
	}
	setup, cancelSetup := context.WithTimeout(legCtx, s.cfg.Timeout)
	defer cancelSetup()
	out, err := reservation.conn.OpenStream(setup)
	if err != nil {
		fail(ConnectionFailed)
		return
	}
	defer out.Close()
	stopOut := context.AfterFunc(legCtx, out.Reset)
	defer stopOut()
	deadline, _ := setup.Deadline()
	_ = out.SetDeadline(deadline)
	if err = wire.Select(out, DatagramStopProtocol); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	if err = writeDatagramMessage(out, datagramMessage{kind: dgRequest, channel: targetChannel, peer: source}); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	reply, err := readDatagramMessage(out)
	if err != nil || reply.kind != dgAccept || reply.channel != targetChannel {
		out.Reset()
		if err == nil && reply.kind == dgFail {
			fail(reply.status)
		} else {
			fail(ConnectionFailed)
		}
		return
	}
	fromSource, err := in.Conn().OpenDatagramChannel(sourceChannel, datagramQueue)
	if err != nil {
		out.Reset()
		fail(ResourceLimit)
		return
	}
	defer fromSource.Close()
	fromTarget, err := reservation.conn.OpenDatagramChannel(targetChannel, datagramQueue)
	if err != nil {
		out.Reset()
		fail(ResourceLimit)
		return
	}
	defer fromTarget.Close()
	if err = writeDatagramMessage(in, datagramMessage{kind: dgAccept, channel: sourceChannel}); err != nil {
		out.Reset()
		return
	}
	// Prove the 1202-byte budget on both legs before either endpoint may send.
	errs := make(chan error, 2)
	for _, conn := range []*host.Conn{in.Conn(), reservation.conn} {
		go func(conn *host.Conn) { errs <- probeBudget(setup, conn) }(conn)
	}
	if err = errors.Join(<-errs, <-errs); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	if err = writeDatagramMessage(out, datagramMessage{kind: dgReady}); err != nil {
		out.Reset()
		fail(ConnectionFailed)
		return
	}
	if err = writeDatagramMessage(in, datagramMessage{kind: dgReady}); err != nil {
		out.Reset()
		return
	}
	cancelSetup()
	_ = in.SetDeadline(time.Time{})
	_ = out.SetDeadline(time.Time{})
	if s.cfg.FullLimit.Duration != 0 {
		var timeout context.CancelFunc
		legCtx, timeout = context.WithTimeout(legCtx, time.Duration(s.cfg.FullLimit.Duration)*time.Second)
		defer timeout()
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		defer cancel()
		forwardDatagrams(legCtx, fromSource, reservation.conn, targetChannel)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		forwardDatagrams(legCtx, fromTarget, in.Conn(), sourceChannel)
	}()
	go func() { defer wg.Done(); defer cancel(); drainControl(in) }()
	go func() { defer wg.Done(); defer cancel(); drainControl(out) }()
	<-legCtx.Done()
	in.Reset()
	out.Reset()
	fromSource.Close()
	fromTarget.Close()
	wg.Wait()
}

// probeBudget retries probes lost to the network but returns immediately when
// the budget itself is too small: fragmentation is never attempted.
func probeBudget(ctx context.Context, conn *host.Conn) error {
	var last error
	for range datagramProbeTrys {
		attempt, cancel := context.WithTimeout(ctx, time.Second)
		_, err := conn.ProbeDatagram(attempt, DatagramPayload)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, host.ErrDatagramTooLarge) || ctx.Err() != nil {
			return err
		}
		last = err
	}
	return last
}
func forwardDatagrams(ctx context.Context, from *host.DatagramChannel, to *host.Conn, channel uint16) {
	for {
		b, err := from.Receive(ctx)
		if err != nil {
			return
		}
		if !validInnerPacket(b) {
			continue
		}
		if err := to.SendDatagram(channel, b); err != nil && !errors.Is(err, host.ErrDatagramTooLarge) {
			return
		}
	}
}

// drainControl ends the leg when its control stream ends. No further control
// messages are defined after READY; any byte is a protocol violation.
func drainControl(s *host.Stream) {
	var b [1]byte
	_, _ = s.Read(b[:])
}
