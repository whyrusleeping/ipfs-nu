package relay_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
	"github.com/whyrusleeping/nu/relay"
)

func TestWSSReservationCircuit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(cert.Certificate())
	tlsCfg := cert.TLS.Clone()
	cert.Close()
	key, _ := wire.GenerateNodeKey()
	declared, _ := wire.ParseMultiaddr("/dns/localhost/tcp/443/tls/ws/p2p/"+key.Public().PeerID().String(), 1024)
	makeHost := func() *host.Host {
		k, _ := wire.GenerateNodeKey()
		h, err := host.New(host.Config{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	a, b := makeHost(), makeHost()
	r, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{Addresses: []wire.Multiaddr{declared}, Admitted: []wire.PeerID{a.ID(), b.ID()}}, WSS: &relay.WSSConfig{Listen: "127.0.0.1:0", TLS: tlsCfg}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, port, _ := net.SplitHostPort(r.WSSAddr().String())
	addr, _ := wire.ParseMultiaddr("/ip4/127.0.0.1/tcp/"+port+"/tls/ws/p2p/"+r.Host.ID().String(), 1024)
	for _, h := range []*host.Host{a, b} {
		c, err := h.DialWSS(ctx, addr, wss.DialOptions{TLSConfig: &tls.Config{RootCAs: roots}})
		if err != nil {
			t.Fatal(err)
		}
		if c.Transport() != "wss" || c.PeerID() != r.Host.ID() {
			t.Fatal("wrong authenticated transport")
		}
		if _, err = c.ExportKeyingMaterial("not-inner", nil, 32); err == nil {
			t.Fatal("outer WSS used as inner QUIC exporter")
		}
	}
	source, err := circuit.NewClient(a, circuit.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := circuit.NewClient(b, circuit.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	reservation, err := target.Reserve(ctx, r.Host.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err = circuit.VerifyVoucher(reservation.Voucher, r.Host.ID(), b.ID(), reservation.Expires); err != nil {
		t.Fatal(err)
	}
	hint, err := circuit.Address(addr, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	_, relayID, peerID, err := circuit.ParseAddress(hint)
	if err != nil || relayID != r.Host.ID() || peerID != b.ID() {
		t.Fatal("WSS circuit hint", err)
	}
	done := make(chan error, 1)
	go func() {
		st, err := target.Accept(ctx)
		if err != nil {
			done <- err
			return
		}
		defer st.Close()
		st.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = io.Copy(st, st)
		done <- err
	}()
	st, err := source.Connect(ctx, r.Host.ID(), b.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(3 * time.Second))
	data := bytes.Repeat([]byte("WSS circuit"), 50000)
	wrote := make(chan error, 1)
	go func() {
		_, err := st.Write(data)
		if err == nil {
			err = st.CloseWrite()
		}
		wrote <- err
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, got) {
		t.Fatalf("data mismatch: %d", len(got))
	}
	if err = <-wrote; err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
