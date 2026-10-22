package wss

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTLSAndCONNECT(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.Copy(c, c)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cfg := &tls.Config{RootCAs: roots}
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", 400)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		defer up.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		client.SetDeadline(time.Now().Add(2 * time.Second))
		up.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(up, rw); up.Close(); close(done) }()
		_, _ = io.Copy(client, up)
		client.Close()
		<-done
	}))
	defer proxy.Close()
	p, _ := url.Parse(proxy.URL)
	for _, via := range []*url.URL{nil, p} {
		c, err := Dial(context.Background(), endpoint, DialOptions{TLSConfig: cfg, Proxy: via})
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		data := bytes.Repeat([]byte("wss"), 50000)
		done := make(chan error, 1)
		go func() { _, err := c.Write(data); done <- err }()
		got := make([]byte, len(data))
		_, err = io.ReadFull(c, got)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, got) {
			t.Fatal("data mismatch")
		}
	}
	if _, err := Dial(context.Background(), endpoint, DialOptions{}); err == nil || !strings.Contains(err.Error(), "TLS certificate/handshake") {
		t.Fatal("untrusted endpoint did not fail clearly", err)
	}
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", http.StatusForbidden) }))
	defer deny.Close()
	p, _ = url.Parse(deny.URL)
	if _, err := Dial(context.Background(), endpoint, DialOptions{TLSConfig: cfg, Proxy: p}); err == nil || !strings.Contains(err.Error(), "CONNECT denied: HTTP 403") {
		t.Fatal(err)
	}
}
func TestFrameValidation(t *testing.T) {
	for _, b := range [][]byte{{0x81, 0}, {0xc2, 0}, {0x82, 0x80, 0, 0, 0, 0}, {0x89, 126, 0, 126}, {0x82, 127, 0, 0, 0, 0, 0, 32, 0, 0}, {0x82, 126, 0, 1, 0}} {
		if _, err := readFrame(bytes.NewReader(b), false); err == nil {
			t.Fatalf("accepted %x", b)
		}
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &Conn{Conn: a, reader: bytes.NewReader([]byte{2, 2, 'a', 'b', 0x80, 2, 'c', 'd'})}
	out := make([]byte, 4)
	if _, err := io.ReadFull(c, out); err != nil || string(out) != "abcd" {
		t.Fatal(string(out), err)
	}
}
func FuzzFrame(f *testing.F) {
	f.Add([]byte{0x82, 0}, false)
	f.Add([]byte{0x82, 0x82, 1, 2, 3, 4, 5, 6}, true)
	f.Add([]byte{0x82, 127, 255, 255, 255, 255, 255, 255, 255, 255}, false)
	f.Fuzz(func(t *testing.T, b []byte, server bool) { _, _ = readFrame(bytes.NewReader(b), server) })
}
