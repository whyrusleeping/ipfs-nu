package kad

import (
	"github.com/whyrusleeping/nu/p2p/wire"
	"testing"
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
