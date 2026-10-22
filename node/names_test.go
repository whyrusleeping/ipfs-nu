package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

func cat(t *testing.T, n *Local, target string) ([]byte, error) {
	t.Helper()
	rc, err := n.Cat(context.Background(), target)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// login logs the node in as the fixture's publisher account.
func login(t *testing.T, n *Local, f *atfix.Fixture) apiv0.Account {
	t.Helper()
	start, err := n.Login(context.Background(), apiv0.LoginRequest{Identifier: atfix.PubHandle})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Browser().Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	final, err := n.WaitOperation(context.Background(), start.Operation.ID, 0)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("login: %v %+v", err, final)
	}
	var acct apiv0.Account
	json.Unmarshal(final.Result, &acct)
	return acct
}

// One node, one account (0045): login stores a session; publish writes a verified name
// record (not readiness); resolve answers from the PDS, then from the cache with its age
// and no PDS request; get/cat/ls and the gateway's /nu/ route bind the name to one CID;
// a local follow becomes a held mirror dataset that switches on a newer publication and
// ends (orphan visible) on a verified deletion; node register and devices round-trip.
func TestNamesPublishResolveFollow(t *testing.T) {
	f := atfix.New(t)
	n := newLocal(t)
	n.Atproto = atproto.New(f.Config())
	ctx := context.Background()

	acct := login(t, n, f)
	if acct.DID != atfix.PubDID || acct.Handle != atfix.PubHandle {
		t.Fatalf("account: %+v", acct)
	}
	if _, err := os.Stat(filepath.Join(n.Repo.Root(), AuthDir, "did_plc_nupublishertestaccount22.json")); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>v1</h1>"), 0o644)
	v1, err := add(t, n, apiv0.AddRequest{Path: dir, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := n.Publish(ctx, apiv0.PublishRequest{Name: "site", Target: v1.CID})
	if err != nil {
		t.Fatal(err)
	}
	if pub.Publication.Kind != "tree" || pub.Publication.Rev == "" || pub.Wait != nil || !strings.Contains(pub.Note, "not replication readiness") {
		t.Fatalf("publish: %+v", pub)
	}

	// Resolution: the publisher fetched its own proof, so the cache answers (no PDS
	// request); --fresh goes to the PDS.
	before := f.Requests["com.atproto.sync.getRecord"]
	res, err := n.ResolveName(ctx, atfix.PubHandle+"/site", apiv0.ResolveRequest{})
	if err != nil || res.Target != v1.CID || res.Source != "cache" || !res.Held || f.Requests["com.atproto.sync.getRecord"] != before {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	res, err = n.ResolveName(ctx, atfix.PubHandle+"/site", apiv0.ResolveRequest{Fresh: true})
	if err != nil || res.Source != "pds" || res.Rev != pub.Publication.Rev || f.Requests["com.atproto.sync.getRecord"] != before+1 {
		t.Fatalf("fresh resolve: %+v %v (%d requests)", res, err, f.Requests["com.atproto.sync.getRecord"]-before)
	}
	if res, err := n.ResolveName(ctx, atfix.PubDID+"/site", apiv0.ResolveRequest{Offline: true}); err != nil || res.Source != "cache" {
		t.Fatalf("offline resolve: %+v %v", res, err)
	}
	if _, err := n.ResolveName(ctx, atfix.PubDID+"/nothere", apiv0.ResolveRequest{}); err == nil || !strings.Contains(err.Error(), "proven absent") {
		t.Fatalf("absent name: %v", err)
	}
	// Name targets in cat/ls and get, and on the gateway.
	if b, err := cat(t, n, atfix.PubHandle+"/site/index.html"); err != nil || string(b) != "<h1>v1</h1>" {
		t.Fatalf("cat by name: %q %v", b, err)
	}
	if l, err := n.Ls(ctx, atfix.PubDID+"/site"); err != nil || len(l.Entries) != 1 || l.Target != v1.CID {
		t.Fatalf("ls by name: %+v %v", l, err)
	}
	gw := httptest.NewServer(gateway.New(n.Content))
	defer gw.Close()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(gw.URL + "/nu/" + atfix.PubHandle + "/site/index.html")
	if err == nil {
		resp.Body.Close()
	}
	if err != nil || resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/tree/"+v1.CID+"/index.html" || resp.Header.Get("X-Nu-Source") != "cache" {
		t.Fatalf("gateway name: %v %v", resp.StatusCode, resp.Header)
	}

	// Follow: a local mirror dataset that holds the current target.
	fl, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubHandle + "/site"})
	if err != nil {
		t.Fatal(err)
	}
	if fl.Dataset != "pub-site" || fl.Mode != "full" || fl.Observed == nil || fl.Observed.Target != v1.CID {
		t.Fatalf("follow: %+v", fl)
	}
	waitFollow := func(state, held string) apiv0.Follow {
		t.Helper()
		var last apiv0.Follow
		for i := 0; i < 100; i++ {
			fs, _ := n.Follows(ctx)
			if len(fs) == 1 {
				last = fs[0]
				if last.State == state && (held == "" || (last.Held != nil && last.Held.Target == held)) {
					return last
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("follow never reached %s/%s: %+v", state, held, last)
		return last
	}
	*fl = waitFollow("held", v1.CID)
	if fl.Held.Rev != pub.Publication.Rev || fl.Content.State != "ready" {
		t.Fatalf("held follow: %+v", fl)
	}
	if ds, err := n.Dataset(ctx, ScopeLocal, "pub-site"); err != nil || ds.Source != "name" || ds.Head.Target != v1.CID {
		t.Fatalf("dataset: %+v %v", ds, err)
	}
	if root, err := n.ResolveDataset(ctx, "pub-site", ""); err != nil || root != v1.CID {
		t.Fatalf("held view: %s %v", root, err)
	}
	if _, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/site"}); err == nil || !strings.Contains(err.Error(), "already followed") {
		t.Fatalf("duplicate follow: %v", err)
	}
	if _, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/site2", Lazy: true}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("lazy follow: %v", err)
	}

	// A newer publication (by the publisher's own daemon) moves the follow's head at once.
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>v2</h1>"), 0o644)
	v2, _ := add(t, n, apiv0.AddRequest{Path: dir, Recursive: true})
	pub2, err := n.Publish(ctx, apiv0.PublishRequest{Name: "site", Target: v2.CID})
	if err != nil || pub2.Publication.Dataset != "pub-site" {
		t.Fatalf("publish v2: %+v %v", pub2, err)
	}
	*fl = waitFollow("held", v2.CID)
	if fl.Held.Rev <= pub.Publication.Rev {
		t.Fatalf("rev order: %+v", fl)
	}
	// Refresh with nothing new changes nothing; a deletion ends the follow visibly.
	if fs, err := n.FollowRefresh(ctx, apiv0.FollowRefreshRequest{}); err != nil || fs[0].State != "held" || fs[0].Held.Target != v2.CID {
		t.Fatalf("refresh: %+v %v", fs, err)
	}
	if _, err := n.Unpublish(ctx, "site", ""); err != nil {
		t.Fatal(err)
	}
	fs, err := n.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: "pub-site"})
	if err != nil || fs[0].State != "ended" || len(fs[0].Orphans) != 1 || fs[0].Orphans[0] != v2.CID || fs[0].Held == nil {
		t.Fatalf("ended follow: %+v %v", fs, err)
	}
	if b, err := cat(t, n, v2.CID+"/index.html"); err != nil || string(b) != "<h1>v2</h1>" {
		t.Fatalf("orphan still held: %q %v", b, err)
	}
	uf, err := n.Unfollow(ctx, atfix.PubHandle+"/site")
	if err != nil || len(uf.Orphans) == 0 {
		t.Fatalf("unfollow: %+v %v", uf, err)
	}
	if fs, _ := n.Follows(ctx); len(fs) != 0 {
		t.Fatalf("follows after unfollow: %+v", fs)
	}

	// Quota stop preserves the held view: a follow with max_size holds a small blob;
	// a larger publication (by its size claim) pauses it with `quota`, the held root
	// stays, and a publication back within the quota resumes.
	small, _ := add(t, n, apiv0.AddRequest{Path: writeTemp(t, []byte("abc")), Raw: true})
	if _, err := n.Publish(ctx, apiv0.PublishRequest{Name: "blob", Target: small.CID}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/blob", Dataset: "blob", MaxSize: 4}); err != nil {
		t.Fatal(err)
	}
	*fl = waitFollow("held", small.CID)
	big, _ := add(t, n, apiv0.AddRequest{Path: writeTemp(t, []byte("abcdefghij")), Raw: true})
	if _, err := n.Publish(ctx, apiv0.PublishRequest{Name: "blob", Target: big.CID}); err != nil {
		t.Fatal(err)
	}
	fs, _ = n.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: "blob"})
	if fs[0].State != "paused" || !strings.Contains(fs[0].Detail, "quota") || fs[0].Held == nil || fs[0].Held.Target != small.CID {
		t.Fatalf("quota pause: %+v", fs[0])
	}
	if _, err := n.Publish(ctx, apiv0.PublishRequest{Name: "blob", Target: small.CID}); err != nil {
		t.Fatal(err)
	}
	if fs, _ = n.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: "blob"}); fs[0].State != "held" {
		t.Fatalf("after resume: %+v", fs[0])
	}
	if _, err := n.Unfollow(ctx, "blob"); err != nil {
		t.Fatal(err)
	}

	// Node registration and devices.
	nr, err := n.NodeRegister(ctx, apiv0.NodeRegisterRequest{Label: "laptop", Seed: true})
	if err != nil || !nr.Self || nr.Peer != n.PeerID().Base36() {
		t.Fatalf("register: %+v %v", nr, err)
	}
	dl, err := n.Devices(ctx, "")
	if err != nil || len(dl.Devices) != 1 || dl.Devices[0].Label != "laptop" || !dl.Devices[0].Self || len(dl.Devices[0].Roles) != 2 {
		t.Fatalf("devices: %+v %v", dl, err)
	}
	if _, err := n.Unpublish(ctx, "blob", ""); err != nil {
		t.Fatal(err)
	}
	names, err := n.Names(ctx, "")
	if err != nil || len(names.Names) != 0 {
		t.Fatalf("names after unpublish: %+v %v", names, err)
	}
	if err := n.Logout(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Publish(ctx, apiv0.PublishRequest{Name: "site", Target: v1.CID}); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("publish without login: %v", err)
	}
}
