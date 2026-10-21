package portmap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestMappingLifecycle(t *testing.T) {
	for _, p := range []Protocol{PCP, PMP, UPnP} {
		t.Run(string(p), func(t *testing.T) {
			f, err := newFake("127.0.0.1", 0, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer f.close()
			cfg := f.config()
			cfg.Protocols = []Protocol{p}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			first, err := c.Refresh(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if first.External.Addr() != f.public || first.External.Port() == 0 || first.Epoch != 1000 || !first.RenewAfter.Before(first.Expires) {
				t.Fatal(first)
			}
			f.epoch.Store(1)
			if err = c.Check(ctx); err != nil {
				t.Fatal(err)
			}
			second, ok := c.Candidate()
			if !ok || !second.Reboot || second.Epoch != 1 || f.maps.Load() != 2 {
				t.Fatalf("reboot: %+v, maps=%d", second, f.maps.Load())
			}
			if err = c.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok = c.Candidate(); ok {
				t.Fatal("released mapping still a candidate")
			}
		})
	}
}
func TestRenewalAndPCPPeer(t *testing.T) {
	f, err := newFake("127.0.0.1", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	cfg := f.config()
	cfg.Protocols = []Protocol{PCP}
	cfg.Remote = netip.MustParseAddrPort("198.18.0.30:9000")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	f.lifetime.Store(1)
	first, err := c.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-c.Changes()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-c.Changes():
	case <-ctx.Done():
		t.Fatal("no renewal before expiry")
	}
	second, ok := c.Candidate()
	if !ok || !time.Now().Before(first.Expires) || !second.Expires.After(first.Expires) || second.Remote != cfg.Remote {
		t.Fatal("invalid renewed peer lease", second)
	}
	cancel()
	<-done
	f.corrupt.Store(true)
	if _, err = c.Refresh(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatal("accepted uncorrelated PCP response", err)
	}
	// IPv6 PEER/MAP codec uses full 128-bit addresses, not NAT-PMP fallback.
	local := netip.MustParseAddrPort("[2001:db8::2]:4001")
	remote := netip.MustParseAddrPort("[2001:db8::3]:9000")
	req := pcpPacket(pcpRequest{Local: local, External: netip.AddrPortFrom(netip.IPv6Unspecified(), 0), Remote: remote, Nonce: c.nonce, Lifetime: 60})
	resp := append([]byte(nil), req...)
	resp[1] |= 128
	ip := local.Addr().As16()
	copy(resp[44:60], ip[:])
	be.PutUint16(resp[42:44], 4001)
	r, err := parsePCP(resp, req, maxPacket)
	if err != nil || r.External != local {
		t.Fatal("IPv6 pinhole codec", r, err)
	}
}
func TestCodecVectorsAndUPnPGuards(t *testing.T) {
	b, err := os.ReadFile("../../testdata/vectors/portmap.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Name, Hex string }
	if err = json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	for i := range nonce {
		nonce[i] = byte(i)
	}
	for _, v := range vectors {
		want, err := hex.DecodeString(v.Hex)
		if err != nil {
			t.Fatal(err)
		}
		got := pmpPacket(4001, 0, 3600)
		if v.Name == "pcp-map-v4" {
			got = pcpPacket(pcpRequest{Local: netip.MustParseAddrPort("10.0.0.2:4001"), External: netip.AddrPortFrom(netip.IPv4Unspecified(), 0), Nonce: nonce, Lifetime: 3600})
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: got %x want %x", v.Name, got, want)
		}
	}
	for _, raw := range []string{"http://other.test/a", "http://127.0.0.2/a", "http://user@127.0.0.1/a"} {
		if _, err := gatewayURL(raw, netip.MustParseAddr("127.0.0.1")); err == nil {
			t.Fatal("unsafe UPnP endpoint", raw)
		}
	}
	if _, _, err := xmlFields([]byte("<a><b><c/></b></a>"), 100, 2); err == nil {
		t.Fatal("XML nesting limit ignored")
	}
}
func FuzzResponses(f *testing.F) {
	f.Add([]byte{0, 128, 0, 0, 0, 0, 0, 1, 198, 18, 0, 20})
	f.Add(make([]byte, 24))
	f.Add([]byte("<root><service/></root>"))
	f.Add([]byte("HTTP/1.1 200 OK\r\nLocation: http://127.0.0.1/\r\n\r\n"))
	req := pcpPacket(pcpRequest{Local: netip.MustParseAddrPort("10.0.0.2:4001"), External: netip.AddrPortFrom(netip.IPv4Unspecified(), 0), Lifetime: 60})
	reply := append([]byte(nil), req...)
	reply[1] |= 128
	f.Add(reply)
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parsePCP(b, req, maxPacket)
		_, _ = parsePMP(b, 0, maxPacket)
		_, _ = parsePMP(b, 1, maxPacket)
		_, _, _ = xmlFields(b, 4096, 16)
		_, _ = parseSSDP(b, 4096)
	})
}
