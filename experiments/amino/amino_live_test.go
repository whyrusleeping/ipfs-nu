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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := libp2p.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r, err := dht.New(h, dht.Mode(dht.ModeClient))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// A bounded wait makes lookup observations meaningful while allowing public routing to settle.
	deadline := time.Now().Add(35 * time.Second)
	for r.RoutingTable().Size() == 0 && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("host=%s routing_table_peers=%d", h.ID(), r.RoutingTable().Size())
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

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
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
	t.Logf("key_type=%s cid=%s provider_peer=%s query_peer=%s announce_latency=%s lookup_latency=%s providers_returned=%d provider_found=%t", label, key, hostA.ID(), hostB.ID(), provided, latency, len(found), gotSelf)
	if !gotSelf {
		t.Errorf("independent client did not retrieve provider record (found %d other providers)", len(found))
	}
}
