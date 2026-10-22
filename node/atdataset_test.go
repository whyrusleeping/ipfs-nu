package node

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// An atproto account as a one-node archive dataset (0054, D21): the first version is a
// verified full export, updates are getRepo?since= diffs applied with ApplySince (a
// full export again when the chain breaks), each verified commit is a version with its
// rev, versions/diff work at record level, ls/cat browse <ds>[@<version>]/<nsid>/<rkey>,
// the gateway's /c/ redirects to /at/<did>@<rev>/, and the redistribution rule flips with
// the account's status: current state may leave the cluster while the account is
// active, retained third-party history never does by default, nothing does once the
// account is deactivated or deleted (history stays for members).
func TestAtrepoArchiveDataset(t *testing.T) {
	f := atfix.New(t)
	n := newLocal(t)
	n.Atproto = atproto.New(f.Config())
	ctx := context.Background()
	pub := f.Pub
	post := func(rkey, text string) {
		pub.Put("app.bsky.feed.post", rkey, map[string]any{"$type": "app.bsky.feed.post", "text": text, "createdAt": "2026-01-01T00:00:00Z"})
	}
	post("3aaa", "first post")
	post("3bbb", "second post")
	pub.Put("app.bsky.actor.profile", "self", map[string]any{"$type": "app.bsky.actor.profile", "displayName": "Pub"})

	d, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "pub", Archive: true, SourceAt: "at://" + atfix.PubHandle})
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "atrepo" || d.Atrepo == nil || d.Atrepo.DID != atfix.PubDID || d.Head == nil || d.Head.Kind != cluster.KindAtrepo || d.Head.Rev != pub.Rev() || len(d.Warnings) != 0 {
		t.Fatalf("create: %+v atrepo %+v warnings %v", d, d.Atrepo, d.Warnings)
	}
	if a := d.Atrepo; a.Mode != "full" || a.Status != "active" || a.Redistribution != RedistCurrentOnly || a.Own || a.PublishHistory {
		t.Fatalf("source state: %+v", a)
	}
	if d.Content.State != cluster.ReadyReady {
		t.Fatalf("one-node readiness: %+v", d.Content)
	}
	vs, err := n.DatasetVersions(ctx, ScopeLocal, "pub")
	if err != nil || len(vs.Versions) != 1 {
		t.Fatalf("versions: %+v %v", vs, err)
	}
	v1 := vs.Versions[0]
	if v1.Kind != cluster.KindAtrepo || v1.Rev != pub.Rev() || v1.Commit != d.Head.Target || v1.Files != 3 || v1.Bytes == 0 || v1.NewBytes != v1.Bytes || v1.Availability != apiv0.AvailComplete {
		t.Fatalf("v1: %+v", v1)
	}

	// Browse the head and a record.
	l, err := n.Ls(ctx, "pub")
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "app.bsky.actor.profile" || l.Entries[1].Size != 2 {
		t.Fatalf("ls: %+v %v", l, err)
	}
	if l, err = n.Ls(ctx, "pub/app.bsky.feed.post"); err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "3aaa" {
		t.Fatalf("ls collection: %+v %v", l, err)
	}
	if s, err := catString(t, n, "pub/app.bsky.feed.post/3aaa"); err != nil || !strings.Contains(s, `"first post"`) {
		t.Fatalf("cat: %q %v", s, err)
	}

	// An update is a since-diff: a create and a delete become a new version; the old
	// version still serves the deleted record, the head does not.
	rev1 := pub.Rev()
	post("3ccc", "third post")
	pub.Delete("app.bsky.feed.post", "3aaa")
	before := f.Requests["com.atproto.sync.getRepo"]
	d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{})
	if err != nil || d.Head.Rev != pub.Rev() || d.Atrepo.Mode != "since" || f.Requests["com.atproto.sync.getRepo"] != before+1 {
		t.Fatalf("update: %+v %v (mode %s)", d, err, d.Atrepo.Mode)
	}
	v2 := d.Version
	if v2 == nil || v2.Parent != v1.TID || v2.Rev != pub.Rev() || v2.Files != 3 || v2.NewBytes == 0 || v2.NewBytes >= v1.Bytes || v2.Availability != apiv0.AvailComplete {
		t.Fatalf("v2: %+v (v1 %+v)", v2, v1)
	}
	diff, err := n.DatasetDiff(ctx, ScopeLocal, "pub", v1.TID, v2.TID)
	if err != nil || strings.Join(diff.Added, ",") != "app.bsky.feed.post/3ccc" || strings.Join(diff.Removed, ",") != "app.bsky.feed.post/3aaa" || len(diff.Changed) != 0 {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	if s, err := catString(t, n, "pub@"+v1.TID+"/app.bsky.feed.post/3aaa"); err != nil || !strings.Contains(s, "first post") {
		t.Fatalf("old version: %q %v", s, err)
	}
	if _, err := catString(t, n, "pub/app.bsky.feed.post/3aaa"); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("deleted record at head: %v", err)
	}
	// No change at the PDS: nothing is fetched.
	before = f.Requests["com.atproto.sync.getRepo"]
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Mode != "status" || f.Requests["com.atproto.sync.getRepo"] != before {
		t.Fatalf("no-op update: %+v %v", d.Atrepo, err)
	}

	// Chain break: the diff lacks a node, so Nu falls back to a full export.
	f.Set(func(f *atfix.Fixture) { f.Pub.BreakSince = true })
	post("3ddd", "fourth post")
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Mode != "full" || d.Head.Rev != pub.Rev() {
		t.Fatalf("chain break: %+v %v", d.Atrepo, err)
	}
	f.Set(func(f *atfix.Fixture) { f.Pub.BreakSince = false })
	vs, _ = n.DatasetVersions(ctx, ScopeLocal, "pub")
	if len(vs.Versions) != 3 || vs.Versions[2].Parent != v2.TID {
		t.Fatalf("after chain break: %+v", vs.Versions)
	}

	// Gateway: /c/<ds>@<version>/… redirects to the held /at/<did>@<rev>/ snapshot, which
	// the dataset's pins make available.
	gw := httptest.NewServer(gateway.New(n.Content))
	defer gw.Close()
	res, _ := gwGet(t, gw, "/c/pub@"+v1.TID+"/app.bsky.feed.post/3aaa")
	loc := "/at/" + atfix.PubDID + "@" + rev1 + "/app.bsky.feed.post/3aaa"
	if res.StatusCode != 302 || res.Header.Get("Location") != loc {
		t.Fatalf("redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, body := gwGet(t, gw, loc); res.StatusCode != 200 || !strings.Contains(string(body), "first post") {
		t.Fatalf("/at/ read of a retained version: %d %s", res.StatusCode, body)
	}

	// Redistribution: the current state may leave (share has no link form for a commit
	// yet, so the answer is the shape), retained history may not (the rule answers).
	if _, err := n.Share(ctx, apiv0.ShareRequest{Target: d.Head.Target}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("share head: %v", err)
	}
	if _, err := n.Share(ctx, apiv0.ShareRequest{Target: v1.Root}); !isCode(err, apiv0.CodeForbidden) || !strings.Contains(err.Error(), "history is private") {
		t.Fatalf("share history: %v", err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: v1.Root}); !isCode(err, apiv0.CodeForbidden) {
		t.Fatalf("public history: %v", err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: d.Head.Target}); !isCode(err, apiv0.CodeNotSupported) {
		t.Fatalf("public head: %v", err)
	}

	// Deactivation: updates stop, history stays, nothing leaves the cluster.
	f.Set(func(f *atfix.Fixture) { f.Pub.Status = "deactivated" })
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Status != "deactivated" || d.Head.Status != "deactivated" || d.Atrepo.Redistribution != RedistPrivate {
		t.Fatalf("deactivated: %+v %v", d.Atrepo, err)
	}
	if _, err := n.Share(ctx, apiv0.ShareRequest{Target: d.Head.Target}); !isCode(err, apiv0.CodeForbidden) || !strings.Contains(err.Error(), "deactivated") {
		t.Fatalf("share while deactivated: %v", err)
	}
	if vs, _ = n.DatasetVersions(ctx, ScopeLocal, "pub"); len(vs.Versions) != 3 {
		t.Fatalf("history after deactivation: %d versions", len(vs.Versions))
	}
	// Reactivation resumes with a since-diff from the held rev.
	f.Set(func(f *atfix.Fixture) { f.Pub.Status = "" })
	post("3eee", "fifth post")
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Status != "active" || d.Head.Status != "" || d.Atrepo.Mode != "since" || d.Head.Rev != pub.Rev() {
		t.Fatalf("reactivated: %+v head %+v %v", d.Atrepo, d.Head, err)
	}
	if _, err := n.Share(ctx, apiv0.ShareRequest{Target: d.Head.Target}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("share after reactivation: %v", err)
	}
	// Deletion: the PDS no longer knows the account; the archive keeps every version.
	f.Set(func(f *atfix.Fixture) { f.Pub.Status = "deleted" })
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Status != "deleted" || d.Head.Status != cluster.AccountDeleted {
		t.Fatalf("deleted: %+v %v", d.Atrepo, err)
	}
	if vs, _ = n.DatasetVersions(ctx, ScopeLocal, "pub"); len(vs.Versions) != 4 || vs.Versions[3].Availability != apiv0.AvailComplete {
		t.Fatalf("history after deletion: %+v", vs.Versions)
	}
	if s, err := catString(t, n, "pub@"+v1.TID+"/app.bsky.feed.post/3aaa"); err != nil || !strings.Contains(s, "first post") {
		t.Fatalf("history read after deletion: %q %v", s, err)
	}

	// Blobs per policy: a second archive of the fixture's own account selects blobs; a
	// record's small blob is fetched and held, an oversized one is skipped and counted,
	// neither makes the version incomplete. A key rotation is re-resolved once.
	f.Set(func(f *atfix.Fixture) { f.Pub.Status = "" })
	small := pub.PutBlob([]byte("png bytes"))
	pub.Put("app.bsky.feed.post", "3fff", map[string]any{"$type": "app.bsky.feed.post", "text": "with images", "createdAt": "2026-01-01T00:00:00Z",
		"embed": map[string]any{"images": []any{atfix.BlobRef(small, "image/png", 9), atfix.BlobRef(pub.Oversized, "video/mp4", 9<<20)}}})
	db, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "pub-blobs", Archive: true, SourceAt: "at://" + atfix.PubDID, Blobs: true})
	if err != nil || db.Atrepo.BlobsFetched != 1 || db.Atrepo.BlobsOversized != 1 || db.Atrepo.BlobsFailed != 0 || len(db.Warnings) != 0 {
		t.Fatalf("blobs: %+v %v %v", db.Atrepo, db.Warnings, err)
	}
	if _, ok, _ := n.Store.Has(small); !ok {
		t.Fatal("small blob not held")
	}
	if vs, _ = n.DatasetVersions(ctx, ScopeLocal, "pub-blobs"); len(vs.Versions) != 1 || vs.Versions[0].Availability != apiv0.AvailComplete || vs.Versions[0].Bytes <= v1.Bytes {
		t.Fatalf("blob version: %+v", vs.Versions)
	}
	pub.RotateKey()
	post("3ggg", "after rotation")
	if db, err = n.DatasetUpdate(ctx, ScopeLocal, "pub-blobs", apiv0.DatasetUpdateRequest{}); err != nil || db.Atrepo.Mode != "since" || db.Head.Rev != pub.Rev() {
		t.Fatalf("key rotation: %+v %v", db.Atrepo, err)
	}

	// Explicit override: history may leave the cluster, and the flag is recorded.
	d2, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "alice", Mirror: true, SourceAt: "at://" + atfix.DID, PublishHistory: true, NSIDs: []string{"app.bsky.feed.like"}})
	if err != nil || d2.Atrepo == nil || !d2.Atrepo.PublishHistory || d2.Atrepo.Redistribution != RedistUnrestricted || d2.History != nil || d2.Head == nil {
		t.Fatalf("override: %+v %+v %v", d2, d2.Atrepo, err)
	}
	if l, err := n.Ls(ctx, "alice"); err != nil || len(l.Entries) != 1 || l.Entries[0].Name != "app.bsky.feed.like" {
		t.Fatalf("nsids scope: %+v %v", l, err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: d2.Head.Target}); !isCode(err, apiv0.CodeNotSupported) {
		t.Fatalf("public with override (allowed by the rule, no route yet): %v", err)
	}
	// Without a preset or with a record URI the source is refused.
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "bad", SourceAt: "at://" + atfix.DID}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("no preset: %v", err)
	}
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "bad", Archive: true, SourceAt: atfix.LikeURI}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("record source: %v", err)
	}
}

// Opt-in live smoke (NU_LIVE=1): archive a small real account from its PDS, then run an
// update, which must be a status check or a since-diff, never a second full export.
func TestLiveAtrepoArchive(t *testing.T) {
	if os.Getenv("NU_LIVE") == "" {
		t.Skip("set NU_LIVE=1 to run against the live network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n := newLocal(t)
	account := os.Getenv("NU_LIVE_ACCOUNT")
	if account == "" {
		account = "why.bsky.world"
	}
	d, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "live", Archive: true, SourceAt: "at://" + account})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Warnings) != 0 || d.Head == nil || d.Atrepo.Mode != "full" {
		t.Fatalf("create: %+v %v", d.Atrepo, d.Warnings)
	}
	t.Logf("archived %s at rev %s (%s)", d.Atrepo.DID, d.Head.Rev, d.Atrepo.Handle)
	if d, err = n.DatasetUpdate(ctx, ScopeLocal, "live", apiv0.DatasetUpdateRequest{}); err != nil {
		t.Fatal(err)
	} else if d.Atrepo.Mode == "full" || d.Atrepo.Error != "" {
		t.Fatalf("update: %+v", d.Atrepo)
	}
	if l, err := n.Ls(ctx, "live"); err != nil || len(l.Entries) == 0 {
		t.Fatalf("ls: %+v %v", l, err)
	}
}
