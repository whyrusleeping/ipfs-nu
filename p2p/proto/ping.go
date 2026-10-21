package proto

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Ping performs one standard 32-byte libp2p ping. RTT excludes negotiation.
func (s *Service) Ping(ctx context.Context, id wire.PeerID) (time.Duration, error) {
	stream, cleanup, err := s.open(ctx, id, PingID, false)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	var sent, received [32]byte
	if _, err := rand.Read(sent[:]); err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := stream.Write(sent[:]); err != nil {
		stream.Reset()
		return 0, err
	}
	if _, err := io.ReadFull(stream, received[:]); err != nil {
		stream.Reset()
		return 0, err
	}
	if !bytes.Equal(sent[:], received[:]) {
		stream.Reset()
		return 0, wire.ErrFormat
	}
	return time.Since(start), nil
}
func (s *Service) pingHandler(stream *host.Stream) {
	var payload [32]byte
	end := time.Now().Add(30 * time.Second)
	for s.ctx.Err() == nil {
		deadline := time.Now().Add(s.config.Timeout)
		if end.Before(deadline) {
			deadline = end
		}
		_ = stream.SetDeadline(deadline)
		if _, err := io.ReadFull(stream, payload[:]); err != nil {
			if err != io.EOF {
				stream.Reset()
			}
			return
		}
		if _, err := stream.Write(payload[:]); err != nil {
			stream.Reset()
			return
		}
	}
}
