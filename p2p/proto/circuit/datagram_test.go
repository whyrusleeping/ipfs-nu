package circuit

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestDatagramMessageVectors(t *testing.T) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	peer := key.Public().PeerID()
	for _, v := range []struct {
		name string
		m    datagramMessage
	}{
		{"request-client", datagramMessage{kind: dgRequest, peer: peer}},
		{"request-relay", datagramMessage{kind: dgRequest, channel: 7, peer: peer}},
		{"accept", datagramMessage{kind: dgAccept, channel: 0x0102}},
		{"ready", datagramMessage{kind: dgReady}},
		{"fail", datagramMessage{kind: dgFail, status: NoReservation}},
	} {
		raw, err := encodeDatagramMessage(v.m)
		if err != nil {
			t.Fatal(v.name, err)
		}
		if int(raw[0]) != len(raw)-1 || raw[1] != datagramVersion || raw[2] != v.m.kind {
			t.Fatalf("%s framing %x", v.name, raw)
		}
		m, err := readDatagramMessage(bytes.NewReader(raw))
		if err != nil || m.kind != v.m.kind || m.channel != v.m.channel || m.peer != v.m.peer || m.status != v.m.status {
			t.Fatalf("%s roundtrip %+v %v", v.name, m, err)
		}
	}
	// Fixed vectors: ACCEPT channel 258, READY, FAIL 204, and rejected encodings.
	for _, h := range []string{"0401020102", "020103", "04010400cc"} {
		raw, _ := hex.DecodeString(h)
		if _, err := readDatagramMessage(bytes.NewReader(raw)); err != nil {
			t.Fatalf("vector %s: %v", h, err)
		}
	}
	for _, h := range []string{"00", "0101", "0401020000", "020105", "03010300", "050101000000", "020203", "04010400" + "00"} {
		raw, _ := hex.DecodeString(h)
		if _, err := readDatagramMessage(bytes.NewReader(raw)); err == nil {
			t.Fatalf("accepted %s", h)
		}
	}
	if _, err := encodeDatagramMessage(datagramMessage{kind: dgAccept}); err == nil {
		t.Fatal("ACCEPT channel 0 encoded")
	}
	if validInnerPacket(nil) || validInnerPacket([]byte{0x00}) || validInnerPacket(make([]byte, 1201)) || !validInnerPacket([]byte{0x40}) {
		t.Fatal("inner packet rule")
	}
}

func FuzzDatagramMessages(f *testing.F) {
	key, _ := wire.GenerateNodeKey()
	peer := key.Public().PeerID().Bytes()
	for _, b := range [][]byte{
		append([]byte{1, 1, 0, 0, byte(len(peer))}, peer...),
		append([]byte{1, 1, 0, 7, byte(len(peer))}, peer...),
		{1, 2, 1, 2}, {1, 2, 0, 0}, {1, 3}, {1, 3, 0}, {1, 4, 0, 204}, {1, 4, 0, 0}, {2, 3}, {1, 5}, {1, 1, 0, 0, 65},
	} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := decodeDatagramMessage(b)
		if err != nil {
			return
		}
		again, err := encodeDatagramMessage(m)
		if err != nil {
			t.Fatal("decoded message does not encode", err)
		}
		if !bytes.Equal(again[1:], b) {
			t.Fatalf("noncanonical %x vs %x", again[1:], b)
		}
	})
}
