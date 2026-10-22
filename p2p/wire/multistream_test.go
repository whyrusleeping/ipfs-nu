package wire_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/varint"
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

// A blocked Write must not hold the lock that Read/Confirm need: full-duplex
// streams (echo under flow control) deadlocked before this regression test.
func TestLazySelectFullDuplexNoDeadlock(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ls, err := wire.NewLazySelect(client, "/echo/1")
	if err != nil {
		t.Fatal(err)
	}
	frame := func(p string) []byte { return append(varint.Append(nil, uint64(len(p)+1)), append([]byte(p), '\n')...) }
	go func() {
		if h, err := wire.ReadProtocol(server, wire.MaxProtocolBytes); err != nil || h != wire.MultistreamHeader {
			return
		}
		if p, err := wire.ReadProtocol(server, wire.MaxProtocolBytes); err != nil || p != "/echo/1" {
			return
		}
		// Respond before draining the pipelined payload, like a peer whose
		// receive window is exhausted while it still sends.
		_, _ = server.Write(append(frame(wire.MultistreamHeader), frame("/echo/1")...))
		_, _ = server.Write([]byte{7})
		buf := make([]byte, 4)
		_, _ = io.ReadFull(server, buf)
	}()
	done := make(chan error, 1)
	go func() { _, err := ls.Write([]byte{1, 2, 3, 4}); done <- err }()
	one := make([]byte, 1)
	readDone := make(chan error, 1)
	go func() { _, err := ls.Read(one); readDone <- err }()
	timer := time.NewTimer(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("write", err)
			}
		case err := <-readDone:
			if err != nil || one[0] != 7 {
				t.Fatal("read", err, one)
			}
		case <-timer.C:
			t.Fatal("full-duplex LazySelect deadlocked")
		}
	}
}
