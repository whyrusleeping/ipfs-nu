package atproto_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// Identity resolution (handle by HTTPS and DNS, DID document), verified record and
// repository fetches, and the failure modes of the contract: tampered CARs, a wrong
// signing key, an unlisted handle, a PDS endpoint behind the SSRF guard.
func TestFetch(t *testing.T) {
	f := atfix.New(t)
	c := atproto.New(f.Config())
	ctx := context.Background()

	t.Run("record via handle", func(t *testing.T) {
		u, err := atproto.ParseURI("at://Alice.Test/" + atfix.LikePath)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := c.GetRecord(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if rec.DID != atfix.DID || rec.Handle != atfix.Handle || rec.Rev == "" || !rec.Commit.Defined() || len(rec.Proof) < 3 {
			t.Fatalf("record %+v", rec)
		}
		v, err := drisl.Decode(rec.Value)
		if err != nil {
			t.Fatal(err)
		}
		if v.(map[string]any)["$type"] != "app.bsky.feed.like" {
			t.Fatalf("value %v", v)
		}
	})
	t.Run("DNS handle not confirmed; absent record", func(t *testing.T) {
		u, _ := atproto.ParseURI("at://" + atfix.DNSName + "/" + atfix.LikePath)
		if _, err := c.GetRecord(ctx, u); err == nil || !strings.Contains(err.Error(), "does not list it") {
			t.Fatalf("unlisted handle: %v", err)
		}
		u, _ = atproto.ParseURI("at://" + atfix.DID + "/app.bsky.feed.like/nope")
		_, err := c.GetRecord(ctx, u)
		var ae *apiv0.Error
		if !errors.As(err, &ae) || ae.Code != apiv0.CodeNotFound {
			t.Fatalf("missing record: %v", err)
		}
	})
	t.Run("repo snapshot", func(t *testing.T) {
		id, err := c.Resolve(ctx, atfix.DID)
		if err != nil {
			t.Fatal(err)
		}
		st := mst.MemStore{}
		var progressed bool
		snap, err := c.GetRepo(ctx, id, st, func(int64) { progressed = true })
		if err != nil {
			t.Fatal(err)
		}
		if snap.Stats.Records == 0 || snap.Rev == "" || snap.Bytes != int64(len(f.Repo)) || !progressed {
			t.Fatalf("snapshot %+v", snap)
		}
		if _, _, ok, err := snap.Repo.Get(atfix.LikePath); err != nil || !ok {
			t.Fatalf("record %s not in snapshot: %v", atfix.LikePath, err)
		}
	})
	t.Run("tampered CAR", func(t *testing.T) {
		f.Set(func(f *atfix.Fixture) { f.Tamper = true })
		defer f.Set(func(f *atfix.Fixture) { f.Tamper = false })
		id, _ := c.Resolve(ctx, atfix.DID)
		if _, err := c.GetRepo(ctx, id, mst.MemStore{}, nil); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("tampered repo: %v", err)
		}
		u, _ := atproto.ParseURI(atfix.LikeURI)
		if _, err := c.GetRecord(ctx, u); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("tampered proof: %v", err)
		}
	})
	t.Run("wrong signing key", func(t *testing.T) {
		f.Set(func(f *atfix.Fixture) { f.KeyMultibase = atfix.OtherKey })
		defer f.Set(func(f *atfix.Fixture) { f.KeyMultibase = atfix.Key })
		c := atproto.New(f.Config()) // fresh cache
		u, _ := atproto.ParseURI(atfix.LikeURI)
		if _, err := c.GetRecord(ctx, u); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("wrong key: %v", err)
		}
		id, _ := c.Resolve(ctx, atfix.DID)
		if _, err := c.GetRepo(ctx, id, mst.MemStore{}, nil); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("wrong key repo: %v", err)
		}
	})
	t.Run("PDS endpoint behind the SSRF guard", func(t *testing.T) {
		f.Set(func(f *atfix.Fixture) { f.PDS = "http://10.1.2.3" })
		defer f.Set(func(f *atfix.Fixture) { f.PDS = "http://pds.test" })
		c := atproto.New(f.Config())
		u, _ := atproto.ParseURI(atfix.LikeURI)
		_, err := c.GetRecord(ctx, u)
		var ae *apiv0.Error
		if !errors.As(err, &ae) || ae.Code != apiv0.CodeForbidden {
			t.Fatalf("private PDS address: %v", err)
		}
	})
	t.Run("uri forms", func(t *testing.T) {
		for _, bad := range []string{"at://", "at://alice.test/notansid", "at://alice.test/app.bsky.feed.post/a/b", "at://did:plc:x@notatid", "http://x"} {
			if _, err := atproto.ParseURI(bad); err == nil {
				t.Errorf("%q parsed", bad)
			}
		}
		u, err := atproto.ParseURI("at://" + atfix.DID + "@3lvwmxcdh2s27/app.bsky.feed.like")
		if err != nil || u.Rev != "3lvwmxcdh2s27" || u.Collection != "app.bsky.feed.like" || u.IsRecord() {
			t.Fatalf("%+v %v", u, err)
		}
	})
}

// TestLive is an opt-in smoke test against the real account (NU_LIVE=1): plc.directory,
// the account's PDS, a signed proof.
func TestLive(t *testing.T) {
	if os.Getenv("NU_LIVE") == "" {
		t.Skip("set NU_LIVE=1 to fetch a real record from the network")
	}
	c := atproto.New(atproto.Config{})
	u, _ := atproto.ParseURI(atfix.LikeURI)
	rec, err := c.GetRecord(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live record %s at %s rev %s from %s (handle %s)", rec.CID, rec.Commit, rec.Rev, rec.PDS, rec.Handle)
}
