package homerelay_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/host"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
	"github.com/whyrusleeping/nu/relay"
)

func TestDeclaredWSSFallbackIndependentBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(cert.Certificate())
	tlsCfg := cert.TLS.Clone()
	cert.Close()
	key, _ := wire.GenerateNodeKey()
	declared, _ := wire.ParseMultiaddr("/dns/localhost/tcp/443/tls/ws/p2p/"+key.Public().PeerID().String(), 1024)
	r, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{Addresses: []wire.Multiaddr{declared}}, WSS: &relay.WSSConfig{Listen: "127.0.0.1:0", TLS: tlsCfg}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, port, _ := net.SplitHostPort(r.WSSAddr().String())
	addr, _ := wire.ParseMultiaddr("/ip4/127.0.0.1/tcp/"+port+"/tls/ws/p2p/"+r.Host.ID().String(), 1024)
	sink, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	_, udpPort, _ := net.SplitHostPort(sink.LocalAddr().String())
	udp, _ := wire.ParseMultiaddr("/ip4/127.0.0.1/udp/"+udpPort+"/quic-v1/p2p/"+r.Host.ID().String(), 1024)
	k, _ := wire.GenerateNodeKey()
	h, err := host.New(host.Config{Key: k})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	identify, err := nuproto.New(h, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer identify.Close()
	client, err := circuit.NewClient(h, circuit.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := time.Now()
	m, err := homerelay.New(ctx, homerelay.Config{Host: h, Client: client, Relays: []wire.Multiaddr{udp, addr}, Timeout: 300 * time.Millisecond, Ping: identify.Ping, WSSOptions: func(wire.PeerID) wss.DialOptions { return wss.DialOptions{TLSConfig: &tls.Config{RootCAs: roots}} }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	state, err := m.Wait(ctx, func(s homerelay.State) bool { return s.Home != "" })
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond || len(state.Relays) != 1 || state.Relays[0].Transport != "wss" || state.Relays[0].UDPError == "" || state.Relays[0].LastError != "" {
		t.Fatalf("fallback state: %+v", state)
	}
	prefix, _, _, err := circuit.ParseAddress(state.Addresses[0])
	if err != nil || prefix.String() != addr.String() {
		t.Fatal("did not publish selected WSS", state.Addresses, err)
	}
	if err := m.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	var endpoints homerelay.Endpoints
	if err := endpoints.Add(udp); err != nil {
		t.Fatal(err)
	}
	if err := endpoints.Add(addr); err != nil {
		t.Fatal(err)
	}
	conflicting, _ := wire.ParseMultiaddr("/dns/other.example/tcp/443/tls/ws/p2p/"+r.Host.ID().String(), 1024)
	if endpoints.Add(conflicting) == nil || endpoints.Contains(conflicting) {
		t.Fatal("second WSS endpoint authorized")
	}
}
