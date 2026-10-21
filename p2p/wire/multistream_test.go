package wire_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestMessageFraming(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors/multistream.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Token, Hex string }
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		got, err := wire.ReadProtocol(bytes.NewReader(unhex(t, v.Hex)), wire.MaxProtocolBytes)
		if err != nil || got != v.Token {
			t.Fatal("multistream vector", v.Token, err)
		}
	}
	var buf bytes.Buffer
	if err := wire.WriteMessage(&buf, []byte("hello"), 5); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{5, 'h', 'e', 'l', 'l', 'o'}) {
		t.Fatal("framing vector")
	}
	b, err := wire.ReadMessage(&buf, 5)
	if err != nil || string(b) != "hello" {
		t.Fatal(err)
	}
	if _, err := wire.ReadMessage(bytes.NewReader([]byte{6}), 5); !errors.Is(err, wire.ErrLimit) {
		t.Fatal("size bound", err)
	}
	if _, err := wire.ReadMessage(bytes.NewReader([]byte{0x80, 0}), 5); err == nil {
		t.Fatal("nonminimal length")
	}
	if _, err := wire.ReadMessage(bytes.NewReader([]byte{5, 1}), 5); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncated body", err)
	}
}
func FuzzMultistreamReader(f *testing.F) {
	f.Add(append([]byte{19}, []byte("/multistream/1.0.0\n")...))
	f.Add([]byte{3, 'n', 'a', '\n'})
	f.Add([]byte{1, '\n'})
	f.Add([]byte{0x80, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = wire.ReadProtocol(bytes.NewReader(b), wire.MaxProtocolBytes)
		_, _ = wire.ReadMessage(bytes.NewReader(b), 8192)
	})
}
