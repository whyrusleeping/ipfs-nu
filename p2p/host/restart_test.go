package host

import (
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestResetKeyAndLiveDuplicate(t *testing.T) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := wire.ParseNodeKey(key.Marshal(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	other, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	if resetKey(key, 4) != resetKey(restored, 4) || resetKey(key, 4) == resetKey(other, 4) || resetKey(key, 4) == resetKey(key, 6) {
		t.Fatal("reset key must persist only for the same identity")
	}
	a, err := New(Config{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(Config{Key: other, Limits: Limits{MaxStreamsPerConnection: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	addr, err := wire.ParseMultiaddr(b.Addrs()[0].String()+"/p2p/"+b.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = a.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	old, err := b.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// An authenticated protocol-NA response proves liveness. Don't evict a live
	// standard peer merely because it does not implement ping.
	b.retireStaleDuplicate(&Conn{peer: a.ID(), outbound: false})
	if old.Context().Err() != nil {
		t.Fatal("live NA peer evicted")
	}
	stream, err := old.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	b.retireStaleDuplicate(&Conn{peer: a.ID(), outbound: false})
	if old.Context().Err() != nil {
		t.Fatal("local stream exhaustion evicted peer")
	}
}
