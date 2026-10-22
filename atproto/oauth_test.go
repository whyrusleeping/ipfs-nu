package atproto_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// callback serves one loopback redirect and returns its query.
func callback(t *testing.T) (string, <-chan url.Values) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan url.Values, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.URL.Query()
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String() + atproto.CallbackPath, ch
}

// Login against the fixture's authorization server: discovery, PAR with a DPoP nonce
// retry, the browser redirect, the token exchange bound to the resolved DID; then an
// authenticated putRecord whose proof verifies, surviving a nonce rotation and an
// expired access token (refresh); a declined authorization fails cleanly; and writes
// outside the granted scope are refused by the PDS.
func TestOAuthLoginAndWrite(t *testing.T) {
	f := atfix.New(t)
	c := atproto.New(f.Config())
	ctx := context.Background()
	redirect, got := callback(t)

	flow, err := c.StartLogin(ctx, atfix.PubHandle, redirect)
	if err != nil {
		t.Fatal(err)
	}
	if flow.DID != atfix.PubDID || !strings.Contains(flow.AuthorizeURL, "request_uri=") {
		t.Fatalf("flow: %+v", flow)
	}
	resp, err := f.Browser().Get(flow.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sess, err := c.FinishLogin(ctx, flow, <-got)
	if err != nil {
		t.Fatal(err)
	}
	if sess.DID != atfix.PubDID || !sess.HasScope("repo:io.ipfs.nu.name") || sess.RefreshToken == "" {
		t.Fatalf("session: %+v", sess)
	}

	saves := 0
	save := func(*atproto.Session) error { saves++; return nil }
	f.Set(func(f *atfix.Fixture) { f.Pub.Rotate(); f.Pub.ExpireAccess = true })
	w, err := c.PutRecord(ctx, sess, "io.ipfs.nu.name", "site", map[string]any{"$type": "io.ipfs.nu.name", "target": "bafkr4iabc", "kind": "blob", "updatedAt": "2026-01-01T00:00:00Z"}, save)
	if err != nil {
		t.Fatal(err)
	}
	if w.URI != "at://"+atfix.PubDID+"/io.ipfs.nu.name/site" || w.Rev == "" || saves == 0 {
		t.Fatalf("write: %+v saves=%d", w, saves)
	}
	rec, err := c.GetRecord(ctx, atproto.URI{Authority: atfix.PubDID, Collection: "io.ipfs.nu.name", RKey: "site"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := drisl.Decode(rec.Value)
	if m, _ := v.(map[string]any); m["target"] != "bafkr4iabc" || rec.Rev != w.Rev {
		t.Fatalf("record %v rev %s/%s", v, rec.Rev, w.Rev)
	}
	if _, err := c.PutRecord(ctx, sess, "app.bsky.feed.post", "x", map[string]any{"$type": "app.bsky.feed.post"}, nil); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("out-of-scope write: %v", err)
	}
	if _, err := c.DeleteRecord(ctx, sess, "io.ipfs.nu.name", "site", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRecord(ctx, atproto.URI{Authority: atfix.PubDID, Collection: "io.ipfs.nu.name", RKey: "site"}); err == nil || !strings.Contains(err.Error(), "proven absent") {
		t.Fatalf("after delete: %v", err)
	}
	if err := c.Revoke(ctx, sess); err != nil || f.Pub.Revoked() != 1 {
		t.Fatalf("revoke: %v %d", err, f.Pub.Revoked())
	}

	// Declined.
	f.Set(func(f *atfix.Fixture) { f.Pub.Deny = true })
	redirect, got = callback(t)
	flow, err = c.StartLogin(ctx, atfix.PubDID, redirect)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = f.Browser().Get(flow.AuthorizeURL)
	resp.Body.Close()
	if _, err := c.FinishLogin(ctx, flow, <-got); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined login: %v", err)
	}
	// A response for another login (wrong state) is rejected.
	if _, err := c.FinishLogin(ctx, flow, url.Values{"state": {"x"}, "code": {"c"}, "iss": {"http://pds.test"}}); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("state check: %v", err)
	}
}
