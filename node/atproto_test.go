package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// runOp runs an operation to completion and decodes its result into out.
func runOp(t *testing.T, n *Local, start func() (*apiv0.Operation, error), out any) error {
	t.Helper()
	o, err := start()
	if err != nil {
		return err
	}
	final, err := n.WaitOperation(context.Background(), o.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != apiv0.StateSucceeded {
		return final.Error
	}
	if err := json.Unmarshal(final.Result, out); err != nil {
		t.Fatal(err)
	}
	return nil
}

// A repository snapshot (`get at://<handle>`) is fetched from the fixture PDS, verified,
// pinned and browsable with ls/cat and on the gateway's /at/ routes; a record fetch
// returns verified JSON with commit and rev; the snapshot exports as a streamable CAR
// that imports into another node (signature verified with a key or online, structure
// only offline) and a tampered CAR is refused; the atrepo re-export is byte-identical.
func TestAtprotoSnapshotBrowseCar(t *testing.T) {
	f := atfix.New(t)
	n := newLocal(t)
	n.Atproto = atproto.New(f.Config())
	ctx := context.Background()

	var snap apiv0.AtSnapshot
	if err := runOp(t, n, func() (*apiv0.Operation, error) {
		return n.AtSnapshot(ctx, apiv0.AtSnapshotRequest{Target: "at://" + atfix.Handle})
	}, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.DID != atfix.DID || snap.Records == 0 || snap.Rev == "" || snap.Profile != "atrepo" {
		t.Fatalf("snapshot %+v", snap)
	}
	pins, _ := n.Store.Pins()
	if len(pins) != 1 || pins[0].Profile != "atrepo" || pins[0].Name != "at://"+atfix.DID+"@"+snap.Rev {
		t.Fatalf("pins %+v", pins)
	}

	// Browse: collections, records, one record (by DID, handle and @rev).
	l, err := n.Ls(ctx, "at://"+atfix.DID)
	if err != nil {
		t.Fatal(err)
	}
	var likes int64
	for _, e := range l.Entries {
		if e.Name == "app.bsky.feed.like" && e.Type == "collection" {
			likes = e.Size
		}
	}
	if likes == 0 || l.Target != "at://"+atfix.DID+"@"+snap.Rev {
		t.Fatalf("collections %+v", l)
	}
	l, err = n.Ls(ctx, "at://"+atfix.Handle+"@"+snap.Rev+"/app.bsky.feed.like")
	if err != nil || int64(len(l.Entries)) != likes || l.Entries[0].Type != "record" {
		t.Fatalf("records: %v %+v", err, l)
	}
	if f.Requests["com.atproto.sync.getRepo"] != 1 {
		t.Fatalf("browsing a held snapshot fetched the repo again: %v", f.Requests)
	}
	rc, err := n.Cat(ctx, atfix.LikeURI)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(body), `"$type":"app.bsky.feed.like"`) || f.Requests["com.atproto.sync.getRecord"] != 0 {
		t.Fatalf("cat from snapshot: %s %v", body, f.Requests)
	}
	if _, err := n.Ls(ctx, "at://"+atfix.DID+"@2222222222222"); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("unknown rev: %v", err)
	}
	// `get`/`dag get` with an at:// target explain the at:// commands.
	if _, err := n.Get(ctx, apiv0.GetRequest{Target: "at://" + atfix.DID, Dir: t.TempDir()}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("get at://: %v", err)
	}

	// Live record fetch: verified, proof kept, same commit as the snapshot (same CAR).
	rec, err := n.AtRecord(ctx, atfix.LikeURI)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Commit != snap.Commit || rec.Rev != snap.Rev || rec.Source != "pds" || !json.Valid(rec.Value) {
		t.Fatalf("record %+v", rec)
	}
	if _, err := n.DagGet(ctx, rec.CID); err != nil {
		t.Fatalf("proof record not held: %v", err)
	}

	// Gateway: inert JSON for records (N30), listings with the CSP, no live fetches.
	gw := httptest.NewServer(gateway.New(n.Content))
	defer gw.Close()
	res, body := gwGet(t, gw, "/at/"+atfix.DID+"/"+atfix.LikePath)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") ||
		res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
		res.Header.Get("Cache-Control") != "no-cache" || !bytes.Contains(body, []byte(`"app.bsky.feed.like"`)) {
		t.Fatalf("gateway record: %d %v %s", res.StatusCode, res.Header, body)
	}
	res, body = gwGet(t, gw, "/at/"+atfix.DID+"@"+snap.Rev+"/")
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("app.bsky.feed.like/")) || res.Header.Get("Cache-Control") != immutable {
		t.Fatalf("gateway collections: %d %v %s", res.StatusCode, res.Header, body)
	}
	if res, _ := gwGet(t, gw, "/at/did:plc:unknown/"); res.StatusCode != 404 {
		t.Fatalf("unheld DID on the gateway: %d", res.StatusCode)
	}
	if f.Requests["com.atproto.sync.getRepo"] != 1 || f.Requests["com.atproto.sync.getRecord"] != 1 {
		t.Fatalf("the gateway fetched live data: %v", f.Requests)
	}

	// Export (atrepo, streamable order) and import into a second node.
	exp, err := n.CarExport(ctx, snap.Commit, "")
	if err != nil {
		t.Fatal(err)
	}
	carBytes, err := io.ReadAll(exp)
	exp.Close()
	if err != nil {
		t.Fatal(err)
	}
	st := mst.MemStore{}
	root, err := atrepo.ReadCAR(bytes.NewReader(carBytes), st, 0)
	if err != nil || root.String() != snap.Commit {
		t.Fatalf("export: %v %s", err, root)
	}
	if _, stats, err := atrepo.Verify(st, root, atrepo.VerifyOptions{}); err != nil || stats.Records != snap.Records {
		t.Fatalf("export verify: %v %+v", err, stats)
	}
	carPath := filepath.Join(t.TempDir(), "alice.car")
	os.WriteFile(carPath, carBytes, 0o644)

	n2 := newLocal(t)
	n2.Atproto = atproto.New(f.Config())
	var imp apiv0.CarImport
	if err := runOp(t, n2, func() (*apiv0.Operation, error) {
		return n2.CarImport(ctx, apiv0.CarImportRequest{Path: carPath, Profile: "atrepo", Pin: true})
	}, &imp); err != nil {
		t.Fatal(err)
	}
	if imp.Signature != "verified" || imp.SignedBy != atfix.Key || imp.DID != atfix.DID || imp.Records != snap.Records || !imp.Pinned || imp.Profile != "atrepo" {
		t.Fatalf("import %+v", imp)
	}
	if l, err := n2.Ls(ctx, "at://"+atfix.DID+"/app.bsky.feed.like"); err != nil || int64(len(l.Entries)) != likes {
		t.Fatalf("imported snapshot browse: %v", err)
	}
	// Offline import without a key: structure verified, signature reported unverified.
	n3 := newLocal(t)
	n3.Atproto = nil
	if err := runOp(t, n3, func() (*apiv0.Operation, error) {
		return n3.CarImport(ctx, apiv0.CarImportRequest{Path: carPath, Offline: true, Pin: true})
	}, &imp); err != nil {
		t.Fatal(err)
	}
	if imp.Profile != "atrepo" || !strings.HasPrefix(imp.Signature, "unverified") || imp.Pinned != true {
		t.Fatalf("offline import %+v", imp)
	}
	// A wrong key refuses; a tampered CAR refuses.
	err = runOp(t, n3, func() (*apiv0.Operation, error) {
		return n3.CarImport(ctx, apiv0.CarImportRequest{Path: carPath, Key: atfix.OtherKey})
	}, &imp)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("wrong key import: %v", err)
	}
	bad := bytes.Clone(carBytes)
	bad[len(bad)-10] ^= 0x55
	badPath := filepath.Join(t.TempDir(), "bad.car")
	os.WriteFile(badPath, bad, 0o644)
	if err := runOp(t, n3, func() (*apiv0.Operation, error) {
		return n3.CarImport(ctx, apiv0.CarImportRequest{Path: badPath, Offline: true})
	}, &imp); err == nil || !strings.Contains(err.Error(), "block") {
		t.Fatalf("tampered import: %v", err)
	}
	// Re-export from the importing node is byte-identical.
	exp2, err := n2.CarExport(ctx, snap.Commit, "atrepo")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(exp2)
	exp2.Close()
	if !bytes.Equal(again, carBytes) {
		t.Fatal("re-export differs")
	}

	// drisl profile: a dag object linking to another exports and imports generically.
	leaf, _ := n.DagPut(ctx, strings.NewReader(`{"v": 1}`))
	rootDoc, _ := n.DagPut(ctx, strings.NewReader(`{"child": {"$link": "`+leaf.CID+`"}}`))
	exp3, err := n.CarExport(ctx, rootDoc.CID, "")
	if err != nil {
		t.Fatal(err)
	}
	drislCar, _ := io.ReadAll(exp3)
	exp3.Close()
	cr, _ := car.NewReader(bytes.NewReader(drislCar), car.Options{})
	var got []string
	for {
		b, _, err := cr.Next()
		if err != nil {
			break
		}
		got = append(got, b.CID.String())
	}
	if len(got) != 2 || got[0] != rootDoc.CID || got[1] != leaf.CID {
		t.Fatalf("drisl export order %v", got)
	}
	drislPath := filepath.Join(t.TempDir(), "dag.car")
	os.WriteFile(drislPath, drislCar, 0o644)
	if err := runOp(t, n3, func() (*apiv0.Operation, error) {
		return n3.CarImport(ctx, apiv0.CarImportRequest{Path: drislPath, Pin: true})
	}, &imp); err != nil || imp.Profile != "drisl" || imp.Objects != 2 {
		t.Fatalf("drisl import: %v %+v", err, imp)
	}
	if _, err := cid.Parse(imp.Root); err != nil {
		t.Fatal(err)
	}
}

const immutable = "public, max-age=31536000, immutable"

func gwGet(t *testing.T, srv *httptest.Server, p string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+p, nil)
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}
