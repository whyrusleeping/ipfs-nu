package autonat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func newHost(t *testing.T, listen string) *host.Host {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := host.Config{Key: key}
	if listen != "" {
		cfg.Listen = []string{listen}
	}
	h, err := host.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func newService(t *testing.T, h *host.Host, cfg Config) *Service {
	t.Helper()
	s, err := New(h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func addr(t *testing.T, text string) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr(text, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func connect(t *testing.T, a, b *host.Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	if _, err := a.Dial(ctx, addr(t, b.Addrs()[0].String()+"/p2p/"+b.ID().String())); err != nil {
		t.Fatal(err)
	}
}

func TestReachability(t *testing.T) {
	client, server := newHost(t, "0.0.0.0:0"), newHost(t, "")
	cli := newService(t, client, Config{AllowPrivateAddresses: true, Timeout: 2 * time.Second})
	newService(t, server, Config{Server: true, AllowPrivateAddresses: true, DialTimeout: 150 * time.Millisecond})
	connect(t, client, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		a := addr(t, strings.Replace(client.Addrs()[0].String(), "0.0.0.0", ip, 1))
		conn, _ := client.Connection(server.ID())
		r, err := cli.Check(ctx, server.ID(), []Request{{Address: a, SendDialData: true}})
		if err != nil || r.Reachability != Reachable || r.Index != 0 || r.Endpoint != conn.RemoteAddr().String() {
			t.Fatalf("%s: %+v, %v", ip, r, err)
		}
	}
	// A UDP sink models a filtered destination: the request connection is live,
	// but no QUIC handshake can complete at the candidate endpoint.
	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	a, err := host.UDPAddress(sink.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	good := addr(t, strings.Replace(client.Addrs()[0].String(), "0.0.0.0", "127.0.0.1", 1))
	r, err := cli.Check(ctx, server.ID(), []Request{{Address: a}, {Address: good}})
	if err != nil || r.Reachability != Unreachable || r.Index != 0 {
		t.Fatal("blocked probe (must not fall back to the reachable second candidate)", r, err)
	}
	// The alternate IP requires dial data. Refusing its cost must not dial.
	alternate := addr(t, strings.Replace(client.Addrs()[0].String(), "0.0.0.0", "127.0.0.2", 1))
	if _, err := cli.Check(ctx, server.ID(), []Request{{Address: alternate}}); !errors.Is(err, ErrDialData) {
		t.Fatal("dial cost", err)
	}
}

func TestLimitsAndPrivatePolicy(t *testing.T) {
	client, server := newHost(t, ""), newHost(t, "")
	if _, err := New(server, Config{Private: true, Server: true}); !errors.Is(err, ErrPrivateMode) || len(server.Protocols()) != 0 {
		t.Fatal("private registration", err, server.Protocols())
	}
	newService(t, server, Config{Server: true, PerPeerPerMinute: 1})
	connect(t, client, server)
	// Default policy refuses even a valid loopback QUIC destination, then rate
	// limits the second request from the same authenticated peer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, want := range []uint32{responseRefused, responseRejected} {
		stream, err := client.OpenStreamLazy(ctx, server.ID(), DialProtocol)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.SetDeadline(time.Now().Add(time.Second))
		if err := writeMessage(stream, message{kind: dialRequest, addrs: [][]byte{client.Addrs()[0].Bytes()}, nonce: 9}); err != nil {
			t.Fatal(err)
		}
		m, err := readMessage(stream)
		stream.Close()
		if err != nil || m.kind != dialResponse || m.status != want {
			t.Fatalf("want %d, got %+v: %v", want, m, err)
		}
	}
	// Exercise global, per-peer, in-flight and dial-data buckets without sleeps.
	l := limiter{active: make(map[wire.PeerID]bool)}
	now := time.Now()
	cfg := Config{RequestsPerMinute: 2, PerPeerPerMinute: 1}
	if !l.admit(client.ID(), now, cfg) || l.admit(client.ID(), now, cfg) {
		t.Fatal("concurrency limit")
	}
	l.release(client.ID())
	if l.admit(client.ID(), now, cfg) || !l.admit(server.ID(), now, cfg) || l.admit("third", now, cfg) {
		t.Fatal("minute limits")
	}
	l.release(server.ID())
	if !l.admit(client.ID(), now.Add(time.Minute), cfg) || !l.admitData(now, 1) || l.admitData(now, 1) || !l.admitData(now.Add(time.Minute), 1) {
		t.Fatal("expiry/data limit")
	}
}

func TestNoProbeBeforeDialData(t *testing.T) {
	client, server := newHost(t, ""), newHost(t, "")
	newService(t, server, Config{Server: true, AllowPrivateAddresses: true, Timeout: time.Second})
	connect(t, client, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	victim, err := net.ListenPacket("udp4", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close()
	a, err := host.UDPAddress(victim.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.OpenStreamLazy(ctx, server.ID(), DialProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if err := writeMessage(stream, message{kind: dialRequest, addrs: [][]byte{a.Bytes()}, nonce: 123}); err != nil {
		t.Fatal(err)
	}
	challenge, err := readMessage(stream)
	if err != nil || challenge.kind != dataRequest || challenge.numBytes < minDialData || challenge.numBytes > maxDialData {
		t.Fatal("challenge", challenge, err)
	}
	// Send a protobuf full of overhead but no data payload. It must not buy a dial.
	if err := writeMessage(stream, message{kind: dataResponse}); err != nil {
		t.Fatal(err)
	}
	if _, err := readMessage(stream); err == nil {
		t.Fatal("invalid payment accepted")
	}
	_ = victim.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var packet [1500]byte
	_, _, err = victim.ReadFrom(packet[:])
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("sent traffic to victim before payment", err)
	}
}

func TestUnverifiedResponses(t *testing.T) {
	client, server := newHost(t, ""), newHost(t, "")
	cli := newService(t, client, Config{AllowPrivateAddresses: true, Timeout: 100 * time.Millisecond})
	connect(t, client, server)
	// A success claim alone, a nonce on the original connection, or a nonce
	// mismatch on a fresh connection is never reachability evidence.
	for _, mode := range []string{"no-nonce", "same-connection", "wrong-nonce", "unknown-status", "excess-cost"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{})
			server.RemoveHandler(DialProtocol)
			if err := server.Handle(DialProtocol, 1, func(stream *host.Stream) {
				defer close(done)
				_ = stream.SetDeadline(time.Now().Add(time.Second))
				m, err := readMessage(stream)
				if err != nil {
					stream.Reset()
					return
				}
				if mode == "same-connection" || mode == "wrong-nonce" {
					h := server
					nonce := m.nonce
					if mode == "wrong-nonce" {
						h = newHostForHandler()
						if h == nil {
							stream.Reset()
							return
						}
						defer h.Close()
						nonce++
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if h != server {
						if _, err := h.Dial(ctx, addrForHandler(client)); err != nil {
							stream.Reset()
							return
						}
					}
					back, err := h.OpenStreamLazy(ctx, client.ID(), DialBackProtocol)
					if err == nil {
						_ = back.SetDeadline(time.Now().Add(time.Second))
						_ = wire.WriteMessage(back, fixed64(nil, 1, nonce), 1024)
						_, err = wire.ReadMessage(back, 1024)
						back.Close()
						if err == nil {
							stream.Reset()
							return
						}
					}
				}
				response := message{kind: dialResponse, status: responseOK, dialStatus: dialOK}
				if mode == "unknown-status" {
					response.dialStatus = 999
				}
				if mode == "excess-cost" {
					response = message{kind: dataRequest, numBytes: maxDialData + 1}
				}
				_ = writeMessage(stream, response)
			}); err != nil {
				t.Fatal(err)
			}
			_, err := cli.Check(context.Background(), server.ID(), []Request{{Address: client.Addrs()[0], SendDialData: true}})
			if err == nil {
				t.Fatal("unverified response accepted")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not finish")
			}
		})
	}
}

// Handler helpers deliberately avoid testing.Fatal from protocol goroutines.
func newHostForHandler() *host.Host {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return nil
	}
	h, _ := host.New(host.Config{Key: key})
	return h
}
func addrForHandler(h *host.Host) wire.Multiaddr {
	a, _ := wire.ParseMultiaddr(fmt.Sprintf("%s/p2p/%s", h.Addrs()[0], h.ID()), 1024)
	return a
}
