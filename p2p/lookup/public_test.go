package lookup_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func localHTTP() *httpsrc.Client {
	return httpsrc.New(httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
}
func publicAddr(t testing.TB, s string) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr(s, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestSignedDirectoryDNSHTTPAndPublish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	peer := key.Public().PeerID()
	server, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{"127.0.0.1:0"}}, Lookup: &relay.LookupConfig{Directory: lookup.DirectoryConfig{Domain: "peers.example.test"}, HTTPListen: "127.0.0.1:0", DNSListen: "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	base := "http://" + server.Lookup.HTTPAddr().String()
	// Enough addresses to require DNS TCP fallback after the 1232-byte UDP cap.
	var addrs []wire.Multiaddr
	for i := 0; i < 20; i++ {
		addrs = append(addrs, publicAddr(t, fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", 4001+i)))
	}
	var seq atomic.Uint64
	publisher, err := lookup.NewPublisher(lookup.PublisherConfig{Client: localHTTP(), BaseURL: base, Key: key, Addresses: func() []wire.Multiaddr { return addrs }, NextSequence: func() (uint64, error) { return seq.Add(1), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = publisher.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	ds, err := lookup.NewDNS(lookup.DNSConfig{Domain: "peers.example.test", Servers: []string{server.Lookup.DNSAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	hs, err := lookup.NewHTTPSource(localHTTP(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []lookup.Source{ds, hs} {
		count := 0
		err = source.Lookup(ctx, peer, func(a lookup.Address) bool {
			count++
			if a.Peer != peer || !strings.HasSuffix(a.Addr.String(), "/p2p/"+peer.String()) {
				t.Error("unbound address", a)
			}
			return true
		})
		if err != nil || count != len(addrs) {
			t.Fatal("lookup", count, err)
		}
	}
	// Existing Resolver consumes both public Sources without changing its defaults.
	service, err := lookup.New(ctx, lookup.Config{DisableMDNS: true, Sources: []lookup.Source{ds, hs}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	count := 0
	for r := range service.Lookup(ctx, peer) {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		count++
	}
	if count != len(addrs) {
		t.Fatal("source deduplication", count)
	}
	envelope, err := lookup.SealPublishedRecord(key, 1, addrs, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Lookup.Directory.Publish(ctx, peer, envelope); !errors.Is(err, lookup.ErrSequence) {
		t.Fatal("replay accepted", err)
	}
	envelope[len(envelope)-1] ^= 1
	if err = server.Lookup.Directory.Publish(ctx, peer, envelope); err == nil {
		t.Fatal("forged record accepted")
	}
	// Publishing on an address-change channel withdraws direct records in relay-only mode.
	relayKey, _ := wire.GenerateNodeKey()
	relayID := relayKey.Public().PeerID()
	circuit := publicAddr(t, "/ip4/127.0.0.1/udp/4444/quic-v1/p2p/"+relayID.String()+"/p2p-circuit/p2p/"+peer.String())
	sealed, err := lookup.SealPublishedRecord(key, 3, append(addrs, circuit), true)
	if err != nil {
		t.Fatal(err)
	}
	record, err := wire.OpenPeerRecord(sealed, lookup.PeerRecordLimits())
	if err != nil || len(record.Addresses) != 1 || record.Addresses[0].String() != circuit.String() {
		t.Fatal("relay-only leak", record, err)
	}
	changes := make(chan struct{}, 1)
	changes <- struct{}{}
	close(changes)
	if err = publisher.Run(ctx, changes); err != nil || seq.Load() != 3 {
		t.Fatal("change publication", seq.Load(), err)
	}
}

func TestDNSADDRRecursionLimitsAndSuffix(t *testing.T) {
	peer := id(t)
	var requests atomic.Int32
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		requests.Add(1)
		r := new(dns.Msg)
		r.SetReply(q)
		records := []string{"dnsaddr=/dnsaddr/leaf.test/tcp/4001/p2p/" + peer.String(), "not a dnsaddr"}
		if q.Question[0].Name == "_dnsaddr.leaf.test." {
			records = []string{"dnsaddr=/ip4/127.0.0.1/tcp/4001/p2p/" + peer.String(), "dnsaddr=/ip4/127.0.0.1/udp/4001/quic-v1/p2p/" + peer.String(), "dnsaddr=/dnsaddr/leaf.test/tcp/4001/p2p/" + peer.String()}
		}
		for _, text := range records {
			mid := len(text) / 2
			r.Answer = append(r.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 5}, Txt: []string{text[:mid], text[mid:]}})
		}
		_ = w.WriteMsg(r)
	})
	ready := make(chan struct{})
	s := &dns.Server{PacketConn: udp, Handler: handler, NotifyStartedFunc: func() { close(ready) }}
	go func() { _ = s.ActivateAndServe() }()
	<-ready
	defer s.Shutdown()
	d, err := lookup.NewDNS(lookup.DNSConfig{Servers: []string{udp.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	root := publicAddr(t, "/dnsaddr/root.test/p2p/"+peer.String())
	var got []lookup.Address
	if err = d.Resolve(context.Background(), peer, root, func(a lookup.Address) bool { got = append(got, a); return true }); err != nil || len(got) != 1 || requests.Load() != 2 {
		t.Fatal("suffix/cycle", got, requests.Load(), err)
	}
	if got[0].Expires.Sub(got[0].Seen) > 5*time.Second {
		t.Fatal("TTL increased")
	}
	l := lookup.DefaultDNSLimits()
	l.MaxQueries = 1
	d, err = lookup.NewDNS(lookup.DNSConfig{Servers: []string{udp.LocalAddr().String()}, Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Resolve(context.Background(), peer, root, func(lookup.Address) bool { return true }); !errors.Is(err, wire.ErrLimit) {
		t.Fatal("query bound ignored", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := requests.Load()
	_ = d.Resolve(ctx, peer, root, func(lookup.Address) bool { return true })
	if requests.Load() != before {
		t.Fatal("canceled lookup sent DNS")
	}
}
func TestRoutingCompatibilityAndPublicationGuards(t *testing.T) {
	peer := id(t)
	b := []byte(`{"Peers":[{"Schema":"future","ID":{"opaque":true},"Addrs":42},{"Schema":"peer","ID":"` + peer.String() + `","Addrs":["/ip4/127.0.0.1/udp/4001/quic-v1"],"extension":{"foo":true}}]}`)
	addrs, err := lookup.ParseRoutingResponse(b, "application/json", peer, lookup.DefaultRoutingLimits())
	if err != nil || len(addrs) != 1 {
		t.Fatal(addrs, err)
	}
	obj, _ := json.Marshal(lookup.PeerJSON{Schema: "peer", ID: peer.String(), Addrs: []string{addrs[0].String()}})
	if addrs, err = lookup.ParseRoutingResponse(append(obj, '\n'), "application/x-ndjson", peer, lookup.DefaultRoutingLimits()); err != nil || len(addrs) != 1 {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	source, _ := lookup.NewHTTPSource(localHTTP(), srv.URL)
	if err = source.Lookup(context.Background(), peer, func(lookup.Address) bool { t.Error("404 emitted a peer"); return false }); err != nil {
		t.Fatal("404 not treated as empty", err)
	}
	key, _ := wire.GenerateNodeKey()
	publisher, _ := lookup.NewPublisher(lookup.PublisherConfig{Client: localHTTP(), BaseURL: srv.URL, Key: key, Addresses: func() []wire.Multiaddr { return nil }, NextSequence: func() (uint64, error) { return 1, nil }})
	if err = publisher.Publish(context.Background()); !errors.Is(err, httpsrc.ErrPolicy) {
		t.Fatal("upload redirect followed", err)
	}
}
func FuzzPublicLookupParsers(f *testing.F) {
	f.Add([]byte("dnsaddr=/ip4/127.0.0.1/udp/4001/quic-v1/p2p/" + peerText))
	f.Add([]byte(`{"Peers":[]}`))
	f.Add([]byte{1, 2, 3})
	peer := id(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = lookup.ParseDNSADDRTXT([]string{string(b)}, 1024)
		_, _ = lookup.ParseRoutingResponse(b, "application/json", peer, lookup.DefaultRoutingLimits())
		_, _ = lookup.ParseRoutingResponse(b, "application/x-ndjson", peer, lookup.DefaultRoutingLimits())
		_, _ = wire.OpenPeerRecord(b, lookup.PeerRecordLimits())
	})
}
