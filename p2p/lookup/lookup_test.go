package lookup_test

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/net/ipv4"
)

const peerText = "12D3KooWBtg3aaRMjxwedh83aGiUkwSxDwUZkzuJcfaqUmo7R3pq"

func id(t testing.TB) wire.PeerID {
	t.Helper()
	id, err := wire.ParsePeerID(peerText)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestCacheHintsRestartParallel(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors/hints.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Link, Peer, Address string }
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		hints, err := lookup.ParseHints(v.Link, lookup.DefaultHintLimits())
		if err != nil || len(hints) != 1 || hints[0].Peer.String() != v.Peer {
			t.Fatal("hint vector", err)
		}
		if v.Address != "" && (len(hints[0].Addresses) != 1 || hints[0].Addresses[0].String() != v.Address) {
			t.Fatal("address vector")
		}
	}
	// A ceiling that only catches hangs: the cache answers a lookup immediately even
	// while a source blocks, so a correct run never approaches it (0074).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link := "nu://bafk-test?peer=" + id(t).Base36() + "&addr=" + url.QueryEscape("/ip4/127.0.0.1/udp/4171/quic-v1")
	hints, err := lookup.ParseHints(link, lookup.DefaultHintLimits())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	path := filepath.Join(t.TempDir(), "peers.json")
	cfg := lookup.CacheConfig{Persistence: lookup.FileStore{Path: path}, Now: func() time.Time { return now }, HalfLife: time.Hour}
	cache, err := lookup.NewCache(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	service, err := lookup.New(ctx, lookup.Config{Cache: cache, DisableMDNS: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RememberHints(ctx, hints); err != nil {
		t.Fatal(err)
	}
	_ = service.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file permissions", err)
	}
	now = now.Add(time.Hour)
	cache, err = lookup.NewCache(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	addresses := cache.Addresses(id(t))
	if len(addresses) != 1 || math.Abs(addresses[0].Confidence-0.3) > 0.00001 || !addresses[0].Seen.Equal(now.Add(-time.Hour)) {
		t.Fatal("cache decay/restart", addresses)
	}
	started := make(chan struct{})
	slow := lookup.SourceFunc(func(ctx context.Context, _ wire.PeerID, _ func(lookup.Address) bool) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	service, err = lookup.New(ctx, lookup.Config{Cache: cache, DisableMDNS: true, Sources: []lookup.Source{slow}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	results := service.Lookup(ctx, id(t))
	<-started
	select {
	case r := <-results:
		if r.Err != nil || r.Address.Peer != id(t) {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal("slow source blocked cache")
	}
	member, err := lookup.HintsFromMember(peerText, []string{"/ip4/127.0.0.1/udp/4171/quic-v1"}, lookup.DefaultHintLimits())
	if err != nil || len(member.Addresses) != 1 {
		t.Fatal("member hints", err)
	}
	if lookup.ServiceName("team") == lookup.ServiceName("") || lookup.ServiceName("team") == lookup.ServiceName("other") {
		t.Fatal("cluster service naming")
	}
}
func multicastLoopback(t *testing.T) net.Interface {
	t.Helper()
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		interfaces, _ := net.Interfaces()
		for _, i := range interfaces {
			if i.Flags&net.FlagLoopback != 0 {
				iface = &i
				break
			}
		}
	}
	if iface == nil {
		t.Skip("no loopback interface")
	}
	group := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 0}
	c, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		t.Skip("multicast unavailable:", err)
	}
	defer c.Close()
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(iface); err != nil {
		t.Skip(err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Skip(err)
	}
	group.Port = c.LocalAddr().(*net.UDPAddr).Port
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.WriteToUDP([]byte("multicast-probe"), group); err != nil {
		t.Skip("loopback multicast send:", err)
	}
	var b [64]byte
	n, _, err := c.ReadFromUDP(b[:])
	if err != nil || string(b[:n]) != "multicast-probe" {
		t.Skip("OS does not loop multicast on loopback:", err)
	}
	return *iface
}
func TestMDNSLoopback(t *testing.T) {
	iface := multicastLoopback(t)
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	makeNode := func() (*host.Host, *lookup.Service) {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		h, err := host.New(host.Config{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		s, err := lookup.New(ctx, lookup.Config{MDNS: lookup.MDNSConfig{Peer: h.ID(), Addresses: h.Addrs, Interfaces: []net.Interface{iface}, ClusterID: t.Name()}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return h, s
	}
	a, la := makeNode()
	b, lb := makeNode()
	for _, pair := range []struct {
		local  *lookup.Service
		remote *host.Host
	}{{la, b}, {lb, a}} {
		query, stop := context.WithCancel(ctx)
		found := false
		for result := range pair.local.Lookup(query, pair.remote.ID()) {
			if result.Err == nil && result.Address.Source == "mdns" {
				expected, _ := lookup.NormalizeAddress(pair.remote.ID(), pair.remote.Addrs()[0])
				if result.Address.Addr.String() == expected.String() {
					found = true
					stop()
					break
				}
			}
		}
		stop()
		if !found {
			t.Fatal("mDNS did not discover peer over working multicast", pair.remote.ID())
		}
	}
}
func FuzzHints(f *testing.F) {
	f.Add("nu://bafk-test?peer=" + peerText + "&addr=%2Fip4%2F127.0.0.1%2Fudp%2F4171%2Fquic-v1")
	f.Add("nu://did:plc:example/site?peer=" + peerText)
	f.Add("nu://cid?addr=%xx")
	f.Add("nu://cid?peer=invalid")
	f.Fuzz(func(t *testing.T, s string) {
		hints, err := lookup.ParseHints(s, lookup.DefaultHintLimits())
		if err != nil {
			return
		}
		for _, h := range hints {
			for _, a := range h.Addresses {
				got, err := lookup.AddressPeer(a)
				if err != nil || got != h.Peer {
					t.Fatal("unbound hint", err)
				}
			}
		}
	})
}
func FuzzCacheFile(f *testing.F) {
	f.Add([]byte(`{"v":1,"entries":[]}`))
	f.Add([]byte(`{"v":2}`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = lookup.DecodeCache(b, 1<<20) })
}
