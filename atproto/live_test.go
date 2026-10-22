package atproto_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/atproto"
)

// The one opt-in live check (NU_LIVE=1): a real handle resolves through DNS/HTTPS and
// plc.directory, a real record proof verifies against the live PDS, and the OAuth
// discovery chain of that PDS validates. Everything else runs against nutest/atfix.
func TestLiveResolveAndDiscovery(t *testing.T) {
	if os.Getenv("NU_LIVE") == "" {
		t.Skip("set NU_LIVE=1 to run against the live network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := atproto.New(atproto.Config{})
	id, err := c.Resolve(ctx, "bsky.app")
	if err != nil {
		t.Fatal(err)
	}
	if id.DID == "" || id.PDS == "" || id.Handle != "bsky.app" {
		t.Fatalf("identity: %+v", id)
	}
	rec, err := c.GetRecord(ctx, atproto.URI{Authority: id.DID, Collection: "app.bsky.actor.profile", RKey: "self"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rev == "" || len(rec.Proof) == 0 {
		t.Fatalf("record: %+v", rec)
	}
	// StartLogin runs discovery and PAR; a real browser is not available, so a usable
	// authorization URL is the success criterion.
	flow, err := c.StartLogin(ctx, "bsky.app", "http://127.0.0.1:1/callback")
	if err != nil {
		t.Fatal(err)
	}
	if flow.AuthorizeURL == "" {
		t.Fatalf("flow: %+v", flow)
	}
	t.Logf("authorize URL: %s", flow.AuthorizeURL)
}
