package wss

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/whyrusleeping/nu/p2p/noise"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/yamux"
)

// SecureConn is an authenticated, multiplexed WSS connection. Its PeerID is
// established by Noise, not by the TLS certificate or the HTTP Host header.
type SecureConn struct {
	*yamux.Session
	key wire.PublicKey
}

func (c *SecureConn) PeerID() wire.PeerID       { return c.key.PeerID() }
func (c *SecureConn) PublicKey() wire.PublicKey { return c.key }

// Secure selects Noise and yamux using standard multistream negotiation. It
// takes ownership of raw on success and failure. The caller must reserve a
// connection slot before invoking it, including for unauthenticated clients.
func Secure(ctx context.Context, raw net.Conn, key *wire.NodeKey, expected wire.PeerID, client bool, cfg yamux.Config) (conn *SecureConn, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	defer func() {
		if err != nil {
			raw.Close()
		}
	}()
	if err = raw.SetDeadline(deadline); err != nil {
		return nil, err
	}
	selectProtocol := func(r net.Conn, proto string) error {
		if client {
			return wire.Select(r, proto)
		}
		_, err := wire.Negotiate(r, func(p string) bool { return p == proto })
		return err
	}
	if err = selectProtocol(raw, noise.ID); err != nil {
		return nil, fmt.Errorf("wss: select Noise: %w", err)
	}
	secured, err := noise.Handshake(ctx, raw, key, expected, client)
	if err != nil {
		return nil, err
	}
	if err = secured.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err = selectProtocol(secured, yamux.ID); err != nil {
		return nil, fmt.Errorf("wss: select yamux: %w", err)
	}
	if err = secured.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	session, err := yamux.New(secured, client, cfg)
	if err != nil {
		return nil, err
	}
	return &SecureConn{Session: session, key: secured.PublicKey()}, nil
}
