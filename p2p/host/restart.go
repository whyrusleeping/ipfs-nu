package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// retireStaleDuplicate runs only after a new connection has authenticated the
// SAME PeerID. A new identity cannot use it to evict an existing connection.
// Same-direction duplicates otherwise fail the simultaneous-dial tie-break even
// after a reboot. Probe the exact old connection, not whichever Host selects next.
func (h *Host) retireStaleDuplicate(next *Conn) {
	old, ok := h.Connection(next.peer)
	if !ok || old.outbound != next.outbound || h.limits.MaxConnectionsPerPeer != 1 {
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, time.Second)
	defer cancel()
	if err := probeConnection(ctx, old); err != nil && h.ctx.Err() == nil {
		// Local resource pressure isn't failed liveness. An authenticated NA
		// response also proves the old endpoint is alive, albeit without ping.
		if errors.Is(err, ErrLimit) || errors.Is(err, wire.ErrNotSupported) {
			return
		}
		_ = old.Close()
	}
}

func probeConnection(ctx context.Context, c *Conn) error {
	stream, err := c.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	deadline, _ := ctx.Deadline()
	_ = stream.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, stream.Reset)
	defer stop()
	if err = wire.Select(stream, "/ipfs/ping/1.0.0"); err != nil {
		return err
	}
	var challenge, response [32]byte
	if _, err = rand.Read(challenge[:]); err != nil {
		return err
	}
	if _, err = stream.Write(challenge[:]); err != nil {
		return err
	}
	if _, err = io.ReadFull(stream, response[:]); err != nil {
		return err
	}
	if !bytes.Equal(challenge[:], response[:]) {
		return wire.ErrFormat
	}
	return nil
}
