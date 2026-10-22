package noise

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestHandshakeRecords(t *testing.T) {
	a, _ := wire.GenerateNodeKey()
	b, _ := wire.GenerateNodeKey()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("noise"), 20000)
	done := make(chan error, 1)
	go func() {
		c, err := Handshake(ctx, right, b, a.Public().PeerID(), false)
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		buf := make([]byte, len(data))
		_, err = io.ReadFull(c, buf)
		if err == nil && !bytes.Equal(buf, data) {
			err = ErrProtocol
		}
		if err == nil {
			_, err = c.Write(buf)
		}
		done <- err
	}()
	c, err := Handshake(ctx, left, a, b.Public().PeerID(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err = c.Write(data); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	if _, err = io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatal("record mismatch")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestIdentityAndTamper(t *testing.T) {
	key, _ := wire.GenerateNodeKey()
	other, _ := wire.GenerateNodeKey()
	static := bytes.Repeat([]byte{7}, 32)
	p := payload(key, static)
	if _, err := verifyPayload(p, static, other.Public().PeerID()); err != ErrIdentity {
		t.Fatal(err)
	}
	static[0] ^= 1
	if _, err := verifyPayload(p, static, ""); err == nil {
		t.Fatal("accepted wrong static key")
	}
	k := make([]byte, 32)
	enc, dec := newCipher(k), newCipher(k)
	msg, _ := enc.crypt([]byte("secret"), nil, true)
	msg[0] ^= 1
	if _, err := dec.crypt(msg, nil, false); err == nil {
		t.Fatal("accepted tampered record")
	}
}
func TestHandshakeCancellation(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	key, _ := wire.GenerateNodeKey()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := Handshake(ctx, a, key, "", false); err == nil {
		t.Fatal("accepted cancelled handshake")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation ignored")
	}
}
func FuzzRecord(f *testing.F) {
	f.Add([]byte{0, 0})
	f.Add([]byte{0, 16, 1})
	f.Add([]byte{255, 255})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = readRecord(bytes.NewReader(b), MaxHandshake) })
}
func FuzzPayload(f *testing.F) {
	key, _ := wire.GenerateNodeKey()
	static := make([]byte, 32)
	f.Add(payload(key, static))
	f.Add([]byte{10, 255, 255})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = verifyPayload(b, static, "") })
}
