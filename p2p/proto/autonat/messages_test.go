package autonat

import (
	"bytes"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestMessageBounds(t *testing.T) {
	tooMany := message{kind: dialRequest}
	for range maxAddresses + 1 {
		tooMany.addrs = append(tooMany.addrs, nil)
	}
	for _, b := range [][]byte{
		tooMany.marshal(),
		(message{kind: dataResponse, data: make([]byte, maxData+1)}).marshal(),
		wire.AppendUint(nil, 1, 0),                                     // oneof must be a nested message
		wire.AppendBytes(nil, dialRequest, wire.AppendUint(nil, 2, 1)), // nonce must be fixed64
		append((message{kind: dialRequest}).marshal(), (message{kind: dialResponse}).marshal()...),
		make([]byte, maxMessage+1),
	} {
		if _, err := parseMessage(b); err == nil {
			t.Fatalf("accepted invalid message (%d bytes)", len(b))
		}
	}
}

// Seeds run as ordinary tests; extended fuzz campaigns belong to post-v0 hardening.
func FuzzMessages(f *testing.F) {
	for _, m := range []message{
		{kind: dialRequest, addrs: [][]byte{{4, 127, 0, 0, 1, 145, 2, 16, 81, 205, 3}}, nonce: 1},
		{kind: dialResponse, status: responseOK, dialStatus: dialOK},
		{kind: dialResponse, status: responseRefused},
		{kind: dataRequest, index: 1, numBytes: maxDialData},
		{kind: dataResponse, data: make([]byte, maxData)},
	} {
		f.Add(uint8(0), m.marshal())
	}
	f.Add(uint8(0), []byte{0x0a, 0xff})
	f.Add(uint8(1), fixed64(nil, 1, 0x1020304050607080))
	f.Add(uint8(1), []byte{8, 1})
	f.Add(uint8(2), []byte{})
	f.Add(uint8(2), []byte{8, 99})
	f.Add(uint8(3), []byte{0x80, 0x80, 0x80, 0x01})
	f.Fuzz(func(t *testing.T, kind uint8, b []byte) {
		switch kind % 4 {
		case 0:
			m, err := parseMessage(b)
			if err == nil {
				if _, err := parseMessage(m.marshal()); err != nil {
					t.Fatal("round trip", err)
				}
			}
		case 1:
			_, _ = parseDialBack(b)
		case 2:
			_ = parseDialBackResponse(b)
		case 3:
			_, _ = readMessage(bytes.NewReader(b))
		}
	})
}
