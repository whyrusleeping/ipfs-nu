package experiments_test

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
	libquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// TestQUICHandshakeStream checks the actual go-libp2p QUIC listener, libp2p TLS,
// mutual identity and a routed bidirectional stream. Negotiation bytes here are
// intentionally literal until Nu's multistream router is introduced by task 0008.
func TestQUICHandshakeStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(libquic.NewTransport), libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"), libp2p.DisableRelay())
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	const proto = protocol.ID("/nu/interop/1")
	seen := make(chan string, 1)
	upstream.SetStreamHandler(proto, func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(time.Second))
		var b [4]byte
		if _, err := io.ReadFull(s, b[:]); err != nil {
			seen <- "read error"
			return
		}
		_, _ = s.Write(b[:])
		seen <- s.Conn().RemotePeer().String()
	})
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	nu, err := host.New(host.Config{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer nu.Close()
	addr, err := wire.ParseMultiaddr(upstream.Addrs()[0].String()+"/p2p/"+upstream.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nu.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if conn.PeerID().String() != upstream.ID().String() {
		t.Fatal("wrong remote identity")
	}
	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	var negotiation []byte
	for _, s := range []string{"/multistream/1.0.0\n", string(proto) + "\n"} {
		negotiation = binary.AppendUvarint(negotiation, uint64(len(s)))
		negotiation = append(negotiation, s...)
	}
	if _, err := stream.Write(append(negotiation, []byte("echo")...)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(negotiation)+4)
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(append(negotiation, []byte("echo")...)) {
		t.Fatalf("unexpected response %q", got)
	}
	select {
	case peer := <-seen:
		if peer != nu.ID().String() {
			t.Fatal("upstream did not authenticate Nu", peer)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
