package experiments

// Cross-checks of Nu's atproto repository core (core/atrepo) against indigo's
// atproto/repo. They live here because indigo's repo packages pull a large IPFS module
// graph into any module that imports them.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	irepo "github.com/bluesky-social/indigo/atproto/repo"
	imst "github.com/bluesky-social/indigo/atproto/repo/mst"
	"github.com/bluesky-social/indigo/atproto/syntax"
	blockformat "github.com/ipfs/go-block-format"
	gocid "github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

func toGo(t *testing.T, c cid.CID) gocid.Cid {
	g, err := gocid.Cast(c.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A cluster-state-style repo signed by a P-256 did:key, exported by Nu in streamable
// order, loads, verifies and reads back in indigo.
func TestIndigoVerifiesNuRepo(t *testing.T) {
	ctx := context.Background()
	priv, _ := atcrypto.GeneratePrivateKeyP256()
	pub, _ := priv.PublicKey()
	st := mst.MemStore{}
	r := atrepo.New(st, pub.DIDKey())
	recs := map[string]cid.CID{}
	for i := range 500 {
		path := fmt.Sprintf("io.ipfs.nu.cluster.member/%s", syntax.NewTIDFromInteger(uint64(i)*7919+1))
		b, _ := drisl.Marshal(map[string]any{"$type": "io.ipfs.nu.cluster.member", "n": int64(i)})
		c, err := r.Put(path, b)
		if err != nil {
			t.Fatal(err)
		}
		recs[path] = c
	}
	head, err := r.Commit(priv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := atrepo.Export(&buf, st, head); err != nil {
		t.Fatal(err)
	}
	commit, repo, err := irepo.LoadRepoFromCAR(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	ipub, err := atcrypto.ParsePublicDIDKey(pub.DIDKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := commit.VerifySignature(ipub); err != nil {
		t.Fatalf("indigo signature check: %v", err)
	}
	if err := repo.MST.Verify(); err != nil {
		t.Fatal(err)
	}
	root, err := repo.MST.RootCID()
	if err != nil || *root != toGo(t, r.Head.Data) {
		t.Fatalf("indigo root %v, Nu %s", root, r.Head.Data)
	}
	for path, c := range recs {
		nsid, rkey, _ := syntax.ParseRepoPath(path)
		_, got, err := repo.GetRecordBytes(ctx, nsid, rkey)
		if err != nil || *got != toGo(t, c) {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

// Randomized trees (with keys mined up to layer 4 so intermediate nodes appear) give the
// same roots in Nu and indigo through inserts and deletes, and each side's diff blocks
// suffice for the other's operation inversion.
func TestIndigoRandomTrees(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 1))
	leaf, leafData, _ := drisl.Sum(map[string]any{"$type": "app.bsky.feed.like", "n": int64(1)})
	gleaf := toGo(t, leaf)
	for iter := range 30 {
		var keys []string
		for len(keys) < 1+rng.IntN(400) {
			k := fmt.Sprintf("app.bsky.feed.post/%s", syntax.NewTIDFromInteger(rng.Uint64()))
			if atrepo.MST.Layer(k) <= rng.IntN(5) {
				keys = append(keys, k)
			}
		}
		st := mst.MemStore{leaf: leafData}
		tr := atrepo.LoadTree(st, cid.CID{})
		it := imst.NewEmptyTree()
		for _, k := range keys {
			_ = tr.Put(k, leaf)
			if _, err := it.Insert([]byte(k), gleaf); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := tr.Flush()
		ib, _ := it.RootCID()
		if *ib != toGo(t, before) {
			t.Fatalf("iter %d: roots differ after inserts", iter)
		}
		// Change a few keys on both sides.
		var ops []atrepo.Op
		var iops []irepo.Operation
		for range 1 + rng.IntN(4) {
			if rng.IntN(2) == 0 {
				k := keys[rng.IntN(len(keys))]
				if ok, _ := tr.Delete(k); ok {
					_, _ = it.Remove([]byte(k))
					ops = append(ops, atrepo.Op{Path: k, Prev: leaf})
					iops = append(iops, irepo.Operation{Path: k, Prev: &gleaf})
				}
			} else {
				k := fmt.Sprintf("app.bsky.feed.like/%s", syntax.NewTIDFromInteger(rng.Uint64()))
				_ = tr.Put(k, leaf)
				_, _ = it.Insert([]byte(k), gleaf)
				ops = append(ops, atrepo.Op{Path: k, CID: leaf})
				iops = append(iops, irepo.Operation{Path: k, Value: &gleaf})
			}
		}
		after, _ := tr.Flush()
		bs := blockstore.NewBlockstore(datastore.NewMapDatastore())
		ia, err := it.WriteDiffBlocks(ctx, bs)
		if err != nil || *ia != toGo(t, after) {
			t.Fatalf("iter %d: roots differ after changes", iter)
		}
		// indigo's diff blocks → Nu inversion.
		islice := mst.MemStore{}
		keysCh, _ := bs.AllKeysChan(ctx)
		for k := range keysCh {
			b, _ := bs.Get(ctx, k)
			c, err := cid.Decode(k.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			// The blockstore keys by multihash and reports raw CIDs; MST nodes are DRISL.
			islice[cid.New(cid.DRISL, c.Multihash())] = b.RawData()
		}
		if got, err := atrepo.Invert(islice, after, ops); err != nil || got != before {
			t.Fatalf("iter %d: Nu inversion over indigo's diff: %v", iter, err)
		}
		// Nu's slice → indigo inversion.
		head := commitOver(t, st, after)
		blocks, _, err := atrepo.Slice(st, before, head)
		if err != nil {
			t.Fatal(err)
		}
		nbs := blockstore.NewBlockstore(datastore.NewMapDatastore())
		for _, b := range blocks {
			blk, _ := blockformat.NewBlockWithCid(b.Data, toGo(t, b.CID))
			_ = nbs.Put(ctx, blk)
		}
		dt, err := imst.LoadTreeFromStore(ctx, nbs, toGo(t, after))
		if err != nil {
			t.Fatal(err)
		}
		for i := range iops {
			if err := irepo.InvertOp(dt, &iops[i]); err != nil {
				t.Fatalf("iter %d: indigo inversion over Nu's slice: %v", iter, err)
			}
		}
		if got, err := dt.RootCID(); err != nil || *got != toGo(t, before) {
			t.Fatalf("iter %d: indigo inverted root %v", iter, err)
		}
	}
}

// commitOver writes a signed commit for data so atrepo.Slice has a head to start from.
func commitOver(t *testing.T, st mst.MemStore, data cid.CID) cid.CID {
	priv, _ := atcrypto.GeneratePrivateKeyK256()
	com := &atrepo.Commit{DID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz", Data: data, Rev: atrepo.NextRev("", time.Now())}
	if err := com.Sign(priv); err != nil {
		t.Fatal(err)
	}
	c, b, err := com.Encode()
	if err != nil {
		t.Fatal(err)
	}
	st[c] = b
	return c
}
