package kad

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestMessageRoundTripAndBounds(t *testing.T) {
	k, _ := wire.GenerateNodeKey()
	id := k.Public().PeerID()
	m := Message{Type: GetProviders, Key: []byte{0x1e, 32, 1, 2}, Providers: []Peer{{ID: id, Addrs: [][]byte{{1, 2, 3}}}}}
	b, e := Encode(m)
	if e != nil {
		t.Fatal(e)
	}
	got, e := Decode(b)
	if e != nil {
		t.Fatal(e)
	}
	if got.Type != m.Type || string(got.Key) != string(m.Key) || len(got.Providers) != 1 || got.Providers[0].ID != id {
		t.Fatalf("roundtrip: %#v", got)
	}
	if _, e = Decode(append(b, 0x80)); e == nil {
		t.Fatal("accepted truncated field")
	}
	if _, e = Encode(Message{Type: Ping, Key: make([]byte, MaxKey+1)}); e == nil {
		t.Fatal("accepted oversized key")
	}
}
func TestTableClosestAndBucketCap(t *testing.T) {
	selfKey, _ := wire.GenerateNodeKey()
	tab := NewTable(selfKey.Public().PeerID())
	for i := 0; i < K+3; i++ {
		k, _ := wire.GenerateNodeKey()
		id := k.Public().PeerID()
		tab.Add(PeerInfo{ID: id})
	}
	for i, bucket := range tab.buckets {
		if len(bucket) > K {
			t.Fatalf("bucket %d has %d peers", i, len(bucket))
		}
	}
	got := tab.Closest([]byte("target"), 3)
	if len(got) != 3 {
		t.Fatalf("closest returned %d", len(got))
	}
}

// A GET_PROVIDERS response for a popular key (more providers, closer peers and addresses
// than our caps, well past MaxMessage) keeps the first valid entries instead of failing;
// requests stay strict, and a response over MaxResponse is still rejected.
func TestOversizedResponseTruncated(t *testing.T) {
	addr := make([]byte, 30)
	entry := func(id []byte, addrs int) []byte {
		e := bytesField(nil, 1, id)
		for range addrs {
			e = bytesField(e, 2, addr)
		}
		return bytesField(e, 2, make([]byte, maxAddrBytes+1)) // dropped, not fatal
	}
	b := uintField(nil, 1, uint64(GetProviders))
	b = bytesField(b, 9, entry([]byte("not a peer id"), 1)) // invalid: skipped
	var ids []wire.PeerID
	for range 100 {
		k, _ := wire.GenerateNodeKey()
		ids = append(ids, k.Public().PeerID())
		b = bytesField(b, 9, entry(ids[len(ids)-1].Bytes(), 40))
	}
	for i := range 30 {
		b = bytesField(b, 8, entry(ids[i].Bytes(), 2))
	}
	if len(b) <= MaxMessage {
		t.Fatalf("fixture is only %d bytes", len(b))
	}
	if _, err := Decode(b); err == nil {
		t.Fatal("a request this large must stay rejected")
	}
	frame := append(varintFrame(len(b)), b...)
	m, err := readMessage(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Providers) != MaxProviders || m.Providers[0].ID != ids[0] || len(m.Closer) != MaxPeers {
		t.Fatalf("kept %d providers (first %s), %d closer", len(m.Providers), m.Providers[0].ID, len(m.Closer))
	}
	for _, p := range m.Providers {
		if len(p.Addrs) != MaxAddrs {
			t.Fatalf("provider kept %d addresses", len(p.Addrs))
		}
	}
	big := bytesField(uintField(nil, 1, uint64(GetProviders)), 11, make([]byte, MaxResponse))
	if _, err := readMessage(bytes.NewReader(append(varintFrame(len(big)), big...))); !errors.Is(err, wire.ErrLimit) {
		t.Fatalf("response over MaxResponse: %v", err)
	}
	if _, err := DecodeResponse(big); !errors.Is(err, wire.ErrLimit) {
		t.Fatalf("DecodeResponse over MaxResponse: %v", err)
	}
}

func varintFrame(n int) []byte { return binary.AppendUvarint(nil, uint64(n)) }
