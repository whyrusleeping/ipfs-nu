package nutest_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node/swarm"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// dhtNetwork starts n nodes forming a small in-process Amino DHT: every node is a server
// (no AutoNAT on loopback) bootstrapped from the others, with loopback addresses allowed
// in routing and provider records.
func dhtNetwork(t *testing.T, n int) *nutest.Network {
	t.Helper()
	return nutest.New(t, n, nutest.Options{Swarm: func(nd *nutest.Node, c *swarm.Config) {
		var peers []kad.Peer
		for _, other := range nd.Network().Nodes {
			if other == nd {
				continue
			}
			a, err := wire.ParseMultiaddr(other.Addrs()[0].String()+"/p2p/"+other.ID.String(), 1024)
			if err != nil {
				t.Fatal(err)
			}
			peers = append(peers, kad.Peer{ID: other.ID, Addrs: [][]byte{a.Bytes()}})
		}
		c.DHT.Mode, c.DHT.Server, c.DHT.AllowPrivateAddresses, c.DHT.Bootstrap = "client", true, true, peers
	}})
}

func providers(t *testing.T, nd *nutest.Node, c cid.CID) []wire.PeerID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nutest.Timeout)
	defer cancel()
	ps, err := nd.Swarm.DHT().FindProviders(ctx, c.Multihash().Bytes())
	if err != nil && len(ps) == 0 {
		t.Logf("find providers of %s at %s: %v", c, nd.Name, err)
	}
	var ids []wire.PeerID
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	return ids
}

// 0072 acceptance: a daemon's pinned CID is found by another daemon through the DHT with
// no link hints; --no-announce content is not provided; a private-mode node runs no DHT.
func TestDHTProvideAndFindWithoutHints(t *testing.T) {
	n := dhtNetwork(t, 4)
	a, b := n.Nodes[0], n.Nodes[3]
	for _, nd := range n.Nodes {
		nutest.Eventually(t, nd.Name+" DHT bootstrap", func() bool {
			st := nd.Swarm.DHTStatus()
			return st.Mode == "server" && st.RoutingPeers >= 2
		})
	}

	announced := a.AddBytes(n.RandBytes(200_000))
	unannounced, err := cid.Parse(a.AddFile(n.RandBytes(50_000), apiv0.AddRequest{NoAnnounce: true}).CID)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := a.Repo.NoAnnounced(); len(list) != 1 || list[0] != unannounced.String() {
		t.Fatalf("no-announce mark not recorded: %v", list)
	}

	// The announce hook provides the file node and its blob; wait for the records.
	nutest.Eventually(t, "provider records for the announced file", func() bool {
		ids := providers(t, b, announced)
		for _, id := range ids {
			if id == a.ID {
				return true
			}
		}
		return false
	})
	nutest.Eventually(t, "provide cycle to finish", func() bool { return !a.Swarm.DHTStatus().LastProvide.IsZero() })
	if ids := providers(t, b, unannounced); len(ids) != 0 {
		t.Fatalf("--no-announce content has providers: %v", ids)
	}

	// B fetches with a hint-less link: providers come from the DHT only.
	out := filepath.Join(t.TempDir(), "out")
	b.Get(n.Link(announced), out)
	if !b.Has(announced) {
		t.Fatal("B does not hold the content after get")
	}
	// (B is connected to A through DHT routing, so a /nu/have probe of connected peers
	// would still find the unannounced file: --no-announce is unlisted, not concealment.)
}

func TestPrivateModeRunsNoDHT(t *testing.T) {
	n := nutest.New(t, 1, nutest.Options{Swarm: func(nd *nutest.Node, c *swarm.Config) {
		c.Private = true
		c.DHT = swarm.DHTConfig{Mode: "client", Server: true, AllowPrivateAddresses: true}
	}})
	nd := n.Nodes[0]
	if nd.Swarm.DHT() != nil || nd.Swarm.DHTStatus().Mode != "off" {
		t.Fatal("private swarm constructed a DHT")
	}
	for _, p := range nd.Swarm.Host().Protocols() {
		if p == kad.Protocol {
			t.Fatal("private swarm registered /ipfs/kad/1.0.0")
		}
	}
}
