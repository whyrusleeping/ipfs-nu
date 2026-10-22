package swarm

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// swarm.dht=auto runs no DHT on a loopback-only listener; "client" runs one, and the
// connectivity owner's AutoNAT hook switches server mode on and off.
func TestDHTModeAndReachabilityHook(t *testing.T) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Key: key, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, Logger: slog.New(slog.DiscardHandler)}
	auto, err := Start(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	if auto.DHT() != nil || auto.DHTStatus().Mode != "off" {
		t.Fatal("auto mode started a DHT on a loopback-only listener")
	}

	cfg := base
	cfg.DHT = DHTConfig{Mode: "client", AllowPrivateAddresses: true}
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.DHT() == nil || s.DHTStatus().Mode != "client" {
		t.Fatalf("client mode: %+v", s.DHTStatus())
	}
	s.dhtReachable(true)
	if s.DHTStatus().Mode != "server" || !hasProtocol(s, kad.Protocol) {
		t.Fatal("public verdict did not enable server mode")
	}
	s.dhtReachable(false)
	if s.DHTStatus().Mode != "client" || hasProtocol(s, kad.Protocol) {
		t.Fatal("lost reachability did not return to client mode")
	}
}

func hasProtocol(s *Swarm, p string) bool {
	for _, x := range s.host.Protocols() {
		if x == p {
			return true
		}
	}
	return false
}

// Only pins with public intent are provided: a cluster dataset pin on a public-mode node
// yields no provider keys, an explicit pin of the same content does (cluster.md: networked
// cluster datasets are member-only).
func TestProvideEnumerationSkipsClusterPins(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blob, err := st.AddBlob(bytes.NewReader([]byte("member-only bytes")), 17)
	if err != nil {
		t.Fatal(err)
	}
	d := &dht{s: &Swarm{cfg: Config{Store: st}}, ctx: context.Background()}
	for _, src := range []string{store.SourceCluster, store.SourceClusterSurplus, store.SourceCollectionSelect, store.SourceCollectionPush} {
		keys, err := d.eligible([]store.Pin{{CID: blob, Mode: store.Direct, Source: src, Name: "photos"}})
		if err != nil || len(keys) != 0 {
			t.Fatalf("%s pin enumerated for providing: %v %v", src, keys, err)
		}
	}
	keys, err := d.eligible([]store.Pin{{CID: blob, Mode: store.Direct, Source: store.SourceExplicit, Name: "ipfs-import:bafy"}})
	if err != nil || len(keys) != 0 {
		t.Fatalf("IPFS import pin enumerated for providing: %v %v", keys, err)
	}
	keys, err = d.eligible([]store.Pin{{CID: blob, Mode: store.Direct, Source: store.SourceExplicit}})
	if err != nil || len(keys) != 1 || keys[0] != blob {
		t.Fatalf("explicit pin not enumerated: %v %v", keys, err)
	}
}
