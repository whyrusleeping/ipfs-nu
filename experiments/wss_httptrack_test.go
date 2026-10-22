package experiments_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	upstreamMux "github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	upstreamNoise "github.com/libp2p/go-libp2p/p2p/security/noise"
	upstreamWS "github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
	"github.com/whyrusleeping/nu/p2p/yamux"
)

func TestWSSNoiseYamuxInteropHTTPTrack(t *testing.T) {
	const proto = "/nu/wss-interop/1"
	data := bytes.Repeat([]byte("WSS Noise yamux interop"), 40000)
	// Reuse httptest's generated loopback certificate without insecure verification.
	certServer := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	serverTLS := certServer.TLS.Clone()
	certServer.Close()
	clientTLS := &tls.Config{RootCAs: roots}
	for _, outbound := range []bool{true, false} {
		t.Run(map[bool]string{true: "nu-to-libp2p", false: "libp2p-to-nu"}[outbound], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key, _ := wire.GenerateNodeKey()
			up, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(upstreamWS.New, upstreamWS.WithTLSConfig(serverTLS), upstreamWS.WithTLSClientConfig(clientTLS)), libp2p.Security(upstreamNoise.ID, upstreamNoise.New), libp2p.Muxer(upstreamMux.ID, upstreamMux.DefaultTransport), libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0/tls/ws"), libp2p.DisableRelay())
			if err != nil {
				t.Fatal(err)
			}
			defer up.Close()
			if outbound {
				up.SetStreamHandler(protocol.ID(proto), func(s network.Stream) {
					defer s.Close()
					s.SetDeadline(time.Now().Add(5 * time.Second))
					_, _ = io.Copy(s, s)
				})
				port, err := up.Addrs()[0].ValueForProtocol(ma.P_TCP)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := wss.Dial(ctx, "wss://"+net.JoinHostPort("127.0.0.1", port)+"/", wss.DialOptions{TLSConfig: clientTLS})
				if err != nil {
					t.Fatal(err)
				}
				id, _ := wire.ParsePeerID(up.ID().String())
				conn, err := wss.Secure(ctx, raw, key, id, true, yamux.Config{})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				stream, err := conn.OpenStream(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				stream.SetDeadline(time.Now().Add(5 * time.Second))
				if err = wire.Select(stream, proto); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := stream.Write(data)
					if err == nil {
						err = stream.CloseWrite()
					}
					done <- err
				}()
				got, err := io.ReadAll(stream)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("data mismatch: %d", len(got))
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
			} else {
				seen := make(chan wire.PeerID, 1)
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, err := wss.Upgrade(w, r)
					if err != nil {
						return
					}
					conn, err := wss.Secure(ctx, raw, key, "", false, yamux.Config{})
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					for {
						stream, err := conn.AcceptStream(ctx)
						if err != nil {
							return
						}
						go func() {
							defer stream.Close()
							stream.SetDeadline(time.Now().Add(5 * time.Second))
							_, err := wire.Negotiate(stream, func(p string) bool { return p == proto })
							if err != nil {
								return
							}
							seen <- conn.PeerID()
							_, _ = io.Copy(stream, stream)
						}()
					}
				}))
				server.TLS = serverTLS.Clone()
				server.StartTLS()
				defer server.Close()
				hostport := strings.TrimPrefix(server.URL, "https://")
				_, port, _ := net.SplitHostPort(hostport)
				addr, _ := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/" + port + "/tls/ws")
				if err = up.Connect(ctx, peer.AddrInfo{ID: peer.ID(key.Public().PeerID()), Addrs: []ma.Multiaddr{addr}}); err != nil {
					t.Fatal(err)
				}
				stream, err := up.NewStream(ctx, peer.ID(key.Public().PeerID()), protocol.ID(proto))
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				stream.SetDeadline(time.Now().Add(5 * time.Second))
				done := make(chan error, 1)
				go func() {
					_, err := stream.Write(data)
					if err == nil {
						err = stream.CloseWrite()
					}
					done <- err
				}()
				got, err := io.ReadAll(stream)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("data mismatch: %d", len(got))
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				select {
				case id := <-seen:
					if id.String() != up.ID().String() {
						t.Fatal("wrong authenticated identity")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}
