package fetch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/nutest/atfix"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
)

func TestAtrepoFallbackPolicyAndRank(t *testing.T) {
	fixture := atfix.New(t)
	pub := fixture.Pub
	data := []byte("selected blob")
	blob := pub.PutBlob(data)
	pub.Put("app.bsky.feed.post", "abc", map[string]any{
		"$type": "app.bsky.feed.post", "image": atfix.BlobRef(blob, "image/png", len(data)),
		"video":     atfix.BlobRef(pub.Oversized, "video/mp4", cid.MaxWholeObject+1),
		"unrelated": map[string]any{"$link": pub.Oversized.String()},
	})
	src := openStore(t, t.TempDir())
	for c, b := range pub.Store {
		if err := src.Put(c, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.Put(blob, data); err != nil {
		t.Fatal(err)
	}
	server := newHost(t, src)
	spare := newHost(t, openStore(t, t.TempDir()))
	for _, noTree := range []bool{false, true} {
		for _, profile := range []string{"atrepo", "atrepo+blobs"} {
			t.Run(profile+map[bool]string{false: "/tree", true: "/fallback"}[noTree], func(t *testing.T) {
				client := newHost(t, nil)
				preferred := countPeer{Peer: connect(t, client, server), noTree: noTree, mu: new(sync.Mutex), opens: map[string]int{}}
				other := countPeer{Peer: connect(t, client, spare), noTree: true, mu: new(sync.Mutex), opens: map[string]int{}}
				dst := openStore(t, t.TempDir())
				f := fetch.New(fetch.Config{Store: dst})
				_, err := f.Fetch(context.Background(), pub.Head, fetch.Options{Profile: profile, Peers: []fetch.Peer{other, preferred}, Rank: func(cid.CID) []wire.PeerID { return []wire.PeerID{preferred.ID()} }})
				if err != nil {
					t.Fatal(err)
				}
				_, held, err := dst.Has(blob)
				if err != nil || held != (profile == "atrepo+blobs") {
					t.Fatalf("blob held %v, profile %s: %v", held, profile, err)
				}
				if preferred.opens[proto.TreeID] != 1 || (!noTree && len(other.opens) != 0) {
					t.Fatalf("source order: preferred %v, other %v", preferred.opens, other.opens)
				}
				if noTree && preferred.opens[proto.BytesID] < 3 {
					t.Fatal("fallback did not fetch structure")
				}
				if !noTree && preferred.opens[proto.BytesID] != map[bool]int{true: 1, false: 0}[profile == "atrepo+blobs"] {
					t.Fatalf("tree fell back for records: %v", preferred.opens)
				}
			})
		}
	}
}
