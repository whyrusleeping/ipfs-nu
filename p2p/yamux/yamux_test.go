package yamux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestFlowControlAndHalfClose(t *testing.T) {
	a, b := net.Pipe()
	client, _ := New(a, true, Config{})
	server, _ := New(b, false, Config{})
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("yamux"), 200000)
	done := make(chan error, 1)
	go func() {
		st, err := server.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		defer st.Close()
		st.SetDeadline(time.Now().Add(2 * time.Second))
		got, err := io.ReadAll(st)
		if err == nil && !bytes.Equal(got, data) {
			err = ErrProtocol
		}
		if err == nil {
			_, err = st.Write(got)
		}
		if err == nil {
			err = st.CloseWrite()
		}
		done <- err
	}()
	st, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = st.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatal("data mismatch")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestLimitsDeadlinesAndReset(t *testing.T) {
	a, b := net.Pipe()
	client, _ := New(a, true, Config{MaxStreams: 1})
	server, _ := New(b, false, Config{MaxStreams: 1})
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.OpenStream(ctx); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(-time.Second))
	if _, err = st.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(time.Second))
	remote.Reset()
	if _, err = st.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatal(err)
	}
}
func TestDeadlineChangeInterruptsQueuedWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	session, _ := New(a, true, Config{})
	defer session.Close()
	go func() { var header [12]byte; _, _ = io.ReadFull(b, header[:]) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := st.Write([]byte("blocked")); done <- err }()
	st.SetWriteDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("changed deadline did not interrupt queued write")
	}
}

func FuzzHeader(f *testing.F) {
	f.Add(header{typ: windowType, flags: syn, id: 1}.encode())
	f.Add(header{typ: dataType, id: 1, length: Window + 1}.encode())
	f.Add(header{typ: pingType, flags: syn, length: 1}.encode())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeHeader(b) })
}
