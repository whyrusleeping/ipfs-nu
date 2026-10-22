package experiments_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	upstream "github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/whyrusleeping/nu/p2p/noise"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestNoiseInteropHTTPTrack(t *testing.T) {
	for _, outbound := range []bool{false, true} {
		t.Run(map[bool]string{false: "libp2p-to-nu", true: "nu-to-libp2p"}[outbound], func(t *testing.T) {
			nuKey, _ := wire.GenerateNodeKey()
			upKey, _, _ := crypto.GenerateEd25519Key(nil)
			upID, _ := peer.IDFromPrivateKey(upKey)
			expected, err := wire.ParsePeerID(upID.String())
			if err != nil {
				t.Fatal(err)
			}
			transport, err := upstream.New(upstream.ID, upKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			data := bytes.Repeat([]byte("interop"), 12000)
			go func() {
				var c net.Conn
				var err error
				if outbound {
					c, err = transport.SecureInbound(ctx, b, peer.ID(nuKey.Public().PeerID()))
				} else {
					c, err = transport.SecureOutbound(ctx, b, peer.ID(nuKey.Public().PeerID()))
				}
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(time.Second))
				buf := make([]byte, len(data))
				_, err = io.ReadFull(c, buf)
				if err == nil {
					_, err = c.Write(buf)
				}
				done <- err
			}()
			c, err := noise.Handshake(ctx, a, nuKey, expected, outbound)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			if c.PeerID() != expected {
				t.Fatal("wrong identity")
			}
			if _, err = c.Write(data); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(data))
			if _, err = io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, got) {
				t.Fatal("payload mismatch")
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
