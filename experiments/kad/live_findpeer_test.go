package kadinterop

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestLiveNuAminoFindPeerPublicBootstrap(t *testing.T) {
	if os.Getenv("NU_AMINO") != "1" {
		t.Skip("set NU_AMINO=1 to query public Amino")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seeds := pinnedLiveBootstrappers(ctx, t)
	if len(seeds) < 2 {
		t.Fatal("need multiple pinned bootstrap peers")
	}
	// The target's identity/address is known from the separate pinned interop
	// inventory, but it is deliberately not inserted into this node's table.
	target := seeds[len(seeds)-1]
	n := newLiveNuNode(t, ctx, seeds[:len(seeds)-1])
	peers, err := n.dht.FindNode(ctx, target.ID)
	found := false
	var foundAddrs [][]byte
	for _, p := range peers {
		if p.ID == target.ID {
			found = true
			foundAddrs = p.Addrs
		}
	}
	t.Logf("FIND_PEER target=%s returned=%d addresses=%d error=%v", target.ID, len(peers), len(foundAddrs), err)
	if !found || len(foundAddrs) == 0 {
		t.Fatalf("public Amino FIND_PEER did not resolve target %s (peers=%v err=%v)", target.ID, peers, err)
	}
	for _, raw := range foundAddrs {
		a, e := wire.MultiaddrFromBytes(raw, 1024)
		if e != nil {
			t.Fatal(e)
		}
		if !kad.IsQUICAddress(a, target.ID) {
			t.Fatalf("non-QUIC public Kubo address returned: %s", a)
		}
	}
}
