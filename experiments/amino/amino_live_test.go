package amino

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	cid "github.com/ipfs/go-cid"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	mh "github.com/multiformats/go-multihash"
	"lukechampine.com/blake3"
)

func TestLiveAminoProviderKeys(t *testing.T) {
	if os.Getenv("NU_AMINO") != "1" {
		t.Skip("set NU_AMINO=1 to contact the public Amino DHT and publish disposable provider records")
	}
	cases := []struct {
		name string
		code uint64
	}{{"sha256", mh.SHA2_256}, {"blake3", 0x1e}, {"swarm-blake3", 0x1e}}
	for _, tc := range cases {
		for run := 0; run < 2; run++ {
			t.Run(fmt.Sprintf("%s/run-%d", tc.name, run+1), func(t *testing.T) { liveRoundTrip(t, tc.name, tc.code, run) })
		}
	}
}

func newDHTClient(t *testing.T) (host.Host, *dht.IpfsDHT) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	h, err := libp2p.New()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	infos := dht.GetDefaultBootstrapPeerAddrInfos()
	if len(infos) == 0 {
		cancel()
		t.Fatal("go-libp2p has no default bootstrap peers")
	}
	// Prefer the fixed TCP bootstrapper (mars) for reproducible TCP connectivity;
	// resolving all DNSADDR bootstrap entries creates a very large, noisy candidate set.
	mars := infos[len(infos)-1]
	r, err := dht.New(h, dht.Mode(dht.ModeClient), dht.BootstrapPeers(mars))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// Force an initial connection: Bootstrap schedules its refresh asynchronously.
	connectCtx, connectCancel := context.WithTimeout(ctx, 15*time.Second)
	err = h.Connect(connectCtx, mars)
	connectCancel()
	connected := 0
	if err != nil {
		t.Logf("bootstrap connect failed peer=%s addrs=%v err=%v", mars.ID, mars.Addrs, err)
	} else {
		connected = 1
		t.Logf("bootstrap connected peer=%s addrs=%v", mars.ID, mars.Addrs)
	}
	if err := r.Bootstrap(ctx); err != nil {
		t.Logf("DHT Bootstrap returned error: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for r.RoutingTable().Size() == 0 && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("host=%s connected_bootstrappers=%d network_peers=%d routing_table_peers=%d", h.ID(), connected, len(h.Network().Peers()), r.RoutingTable().Size())
	cancel()
	if r.RoutingTable().Size() == 0 {
		t.Fatalf("unable to bootstrap DHT: no routing peers; connected=%d network_peers=%d", connected, len(h.Network().Peers()))
	}
	return h, r
}

func liveRoundTrip(t *testing.T, label string, code uint64, run int) {
	hostA, a := newDHTClient(t)
	hostB, b := newDHTClient(t) // fresh identity and independent routing table
	_ = hostA
	var payload [32]byte
	if _, err := rand.Read(payload[:]); err != nil {
		t.Fatal(err)
	}
	if label == "swarm-blake3" {
		payload = blake3.Sum256([]byte("nu.follow\x00did:plc:ewvi7nxzyoun6zhxrhs64oiz\x00amino-spike-dataset"))
	}
	if label == "blake3" {
		payload = blake3.Sum256(append([]byte("nu-amino-spike-"), payload[:]...))
	}
	if label == "sha256" {
		payload = sha256.Sum256(payload[:])
	}
	mhbytes, err := mh.Encode(payload[:], code)
	if err != nil {
		t.Fatal(err)
	}
	key := cid.NewCidV1(cid.Raw, mh.Multihash(mhbytes))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	if err := a.Provide(ctx, key, true); err != nil {
		t.Fatalf("provide %s (%s): %v", key, label, err)
	}
	provided := time.Since(start)
	start = time.Now()
	ch := b.FindProvidersAsync(ctx, key, 5)
	var found []peer.AddrInfo
	for info := range ch {
		found = append(found, info)
		if len(found) == 5 {
			break
		}
	}
	latency := time.Since(start)
	gotSelf := false
	for _, p := range found {
		if p.ID == hostA.ID() {
			gotSelf = true
		}
	}
	var advertised []string
	for _, p := range found {
		if p.ID == hostA.ID() {
			for _, addr := range p.Addrs {
				advertised = append(advertised, addr.String())
			}
		}
	}
	t.Logf("key_type=%s cid=%s provider_peer=%s query_peer=%s announce_latency=%s lookup_latency=%s providers_returned=%d provider_found=%t provider_addrs=%v", label, key, hostA.ID(), hostB.ID(), provided, latency, len(found), gotSelf, advertised)
	if !gotSelf {
		t.Errorf("independent client did not retrieve provider record (found %d other providers)", len(found))
	}
}
