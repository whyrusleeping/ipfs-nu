package homerelay

import (
	"bytes"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func peerID(t *testing.T) wire.PeerID {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().PeerID()
}
func TestTableCodecAndChoice(t *testing.T) {
	far, near, other := peerID(t), peerID(t), peerID(t)
	local := []TableEntry{{Peer: far, RTT: 40 * time.Millisecond}, {Peer: near, RTT: 2 * time.Millisecond}, {Peer: other}}
	b, err := EncodeTable(local)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeTable(b)
	if err != nil || len(back) != 3 || back[0].RTT != 40*time.Millisecond || back[2].RTT != 0 || back[1].Peer != near {
		t.Fatal(back, err)
	}
	if b[0] != 1 || b[1] != 3 || b[2] != 0 || b[3] != 40 || b[4] != 0 {
		t.Fatalf("encoding %x", b)
	}
	for _, bad := range [][]byte{{}, {2, 0}, {1, 17}, {1, 1, 0, 1, 1, 1, 0}, {1, 1, 0, 1, 0, 0}, append(append([]byte{1, 1, 0, 1, 0, byte(len(near.Bytes()))}, near.Bytes()...), 0)} {
		if _, err := DecodeTable(bad); err == nil {
			t.Fatalf("accepted %x", bad)
		}
	}
	dup := append([]byte{1, 2}, b[2:2+4+len(far.Bytes())]...)
	dup = append(dup, b[2:2+4+len(far.Bytes())]...)
	if _, err := DecodeTable(dup); err == nil {
		t.Fatal("duplicate accepted")
	}
	if !bytes.Equal(b, b) {
		t.Fatal()
	}
	remote := []TableEntry{{Peer: far, RTT: 45 * time.Millisecond}, {Peer: near, RTT: 3 * time.Millisecond}, {Peer: other, RTT: time.Millisecond}}
	best, sum, better := Choose(local, remote, far)
	if best != near || sum != 5*time.Millisecond || !better {
		t.Fatal(best, sum, better)
	}
	if best, _, better := Choose(local, remote, near); best != near || better {
		t.Fatal("switched away from the best relay")
	}
	// Hysteresis: 10% better is not enough.
	close := []TableEntry{{Peer: far, RTT: 40 * time.Millisecond}, {Peer: near, RTT: 36 * time.Millisecond}}
	if _, _, better := Choose(close, []TableEntry{{Peer: far, RTT: 40 * time.Millisecond}, {Peer: near, RTT: 40 * time.Millisecond}}, far); better {
		t.Fatal("switched within hysteresis")
	}
	// Unknown current relay: switch to any measured pair.
	if _, _, better := Choose(local, remote[1:2], other); !better {
		t.Fatal("did not leave an unmeasured relay")
	}
	if best, _, _ := Choose(nil, remote, far); best != "" {
		t.Fatal("chose without local measurements")
	}
}
func FuzzTable(f *testing.F) {
	k, _ := wire.GenerateNodeKey()
	id := k.Public().PeerID().Bytes()
	f.Add([]byte{1, 0})
	f.Add(append([]byte{1, 1, 0, 5, 0, byte(len(id))}, id...))
	f.Add(append([]byte{1, 1, 255, 255, 0, byte(len(id))}, id...))
	f.Fuzz(func(t *testing.T, b []byte) {
		entries, err := DecodeTable(b)
		if err != nil {
			return
		}
		again, err := EncodeTable(entries)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("noncanonical %x vs %x (%v)", again, b, err)
		}
	})
}
