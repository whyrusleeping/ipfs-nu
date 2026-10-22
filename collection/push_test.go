package collection_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

// egress counts name lookups and dials of URL imports per host name, and resolves the
// test names to the local test server.
type egress struct {
	mu      sync.Mutex
	lookups map[string]int
	dials   int
}

func (e *egress) hook(srv string) func(*httpsrc.Config) {
	return func(c *httpsrc.Config) {
		c.AllowHTTP = true
		c.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
		c.LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
			e.mu.Lock()
			e.lookups[host]++
			e.mu.Unlock()
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		c.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			e.mu.Lock()
			e.dials++
			e.mu.Unlock()
			if addr != srv {
				return nil, fmt.Errorf("unexpected dial to %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
}

func (e *egress) count(host string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lookups[host]
}

// TestItemPush: a producer pushes every item to two receiving members (URL files from an
// allowlisted host plus an upload), is interrupted, restarts and resends everything
// while one member restarts; both members' heads hold every item exactly once and the
// union has no conflicts. Egress: unlisted hosts and redirects to them are refused before
// any lookup; 404s and checksum mismatches fail the push; conflicts need replace; DELETE
// removes the item; the push log lists each path once.
func TestItemPush(t *testing.T) {
	for k, v := range map[*time.Duration]time.Duration{&node.HeadInterval: 100 * time.Millisecond, &node.IngestBackoff: 20 * time.Millisecond} {
		old := *k
		*k = v
		t.Cleanup(func() { *k = old })
	}
	var stall atomic.Bool
	var ranged atomic.Int32
	stalled := make(chan struct{})
	scene := func(i int) []byte { return bytes.Repeat([]byte(fmt.Sprintf("scene %d pixels ", i)), 20000+i) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var i int
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://secret.test/x", http.StatusFound)
		default:
			if _, err := fmt.Sscanf(r.URL.Path, "/items/%d/visual.tif", &i); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"s%d"`, i))
			if r.Header.Get("Range") != "" {
				ranged.Add(1)
			}
			if i == 3 && stall.CompareAndSwap(false, true) {
				// The first fetch of item 3 stalls mid-body until its member restarts.
				data := scene(i)
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				_, _ = w.Write(data[:len(data)/2])
				w.(http.Flusher).Flush()
				close(stalled)
				<-r.Context().Done()
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(scene(i)))
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	eg := &egress{lookups: map[string]int{}}
	node.IngestHTTP = eg.hook(srv.Listener.Addr().String())
	t.Cleanup(func() { node.IngestHTTP = nil })
	base := "http://scenes.test:" + port

	n := nutest.New(t, 3)
	a, b, c := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "collgs", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	join(t, a, b)
	join(t, a, c)
	wa, _ := a.Local.NetCluster().NewKey()
	wb, _ := b.Local.NetCluster().NewKey()
	if _, err := a.Local.NetCluster().Write(
		cluster.Op{Collection: cluster.NSKey, RKey: wa, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSKey, RKey: wb, Value: &cluster.Key{Role: cluster.RoleWriter}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "s2", Collection: true, Writers: []string{wa, wb},
		Facets: map[string]apiv0.FacetDecl{"cloud": {Ptr: "/stac/properties/eo:cloud_cover", Type: "number"}},
		Ingest: &apiv0.DatasetIngest{Hosts: []string{"scenes.test"}, Attempts: 3}}); err != nil {
		t.Fatal(err)
	}
	nutest.Eventually(t, "writer keys and dataset on b", func() bool {
		_, ok := b.Local.NetCluster().View().Dataset("s2")
		return ok && b.Local.NetCluster().View().Keys[wb] != nil
	})
	if _, err := c.Local.CollectionPush(ctx(t), "s2", apiv0.CollectionPushRequest{Path: "x", Meta: []byte(`{}`),
		Files: map[string]apiv0.CollectionPushFile{"f": {URL: base + "/items/0/visual.tif"}}}, 0); apiCode(err) != apiv0.CodeForbidden {
		t.Fatalf("push to a member without a writer key: %v", err)
	}

	const items = 8
	thumb := map[*nutest.Node]string{}
	for _, nd := range []*nutest.Node{a, b} {
		u, err := nd.Local.CollectionUpload(ctx(t), "s2", strings.NewReader("thumbnail"), -1)
		if err != nil {
			t.Fatal(err)
		}
		thumb[nd] = u.CID
	}
	push := func(nd *nutest.Node, i int) *apiv0.CollectionPush {
		t.Helper()
		sum := sha256.Sum256(scene(i))
		p, err := nd.Local.CollectionPush(ctx(t), "s2", apiv0.CollectionPushRequest{
			Path: fmt.Sprintf("2026/10/21/S2B_%02d", i),
			Meta: []byte(fmt.Sprintf(`{"stac": {"id": "S2B_%02d", "properties": {"eo:cloud_cover": %d.5}}}`, i, i)),
			Files: map[string]apiv0.CollectionPushFile{
				"visual.tif": {URL: fmt.Sprintf("%s/items/%d/visual.tif", base, i), Checksum: "1220" + hex.EncodeToString(sum[:])},
				"thumb.txt":  {Upload: thumb[nd]},
			}}, 0)
		if err != nil {
			t.Fatalf("push %d to %s: %v", i, nd.Name, err)
		}
		return p
	}
	ids := map[string]string{}
	for i := range items / 2 {
		for _, nd := range []*nutest.Node{a, b} {
			p := push(nd, i)
			ids[nd.Name+p.Path] = p.ID
		}
	}
	// The producer is interrupted here; both members restart with a download in flight
	// (it resumes with Range afterwards). The producer then resends everything.
	select {
	case <-stalled:
	case <-time.After(nutest.Timeout):
		t.Fatal("item 3 was never fetched")
	}
	a.Restart()
	b.Restart()
	for i := range items {
		for _, nd := range []*nutest.Node{a, b} {
			p := push(nd, i)
			if id, ok := ids[nd.Name+p.Path]; ok && (p.ID != id || !p.Duplicate) {
				t.Fatalf("resend to %s answered %+v, want the accepted push %s", nd.Name, p, id)
			}
			ids[nd.Name+p.Path] = p.ID
		}
	}
	for _, nd := range []*nutest.Node{a, b} {
		for i := range items {
			path := fmt.Sprintf("2026/10/21/S2B_%02d", i)
			p, err := nd.Local.Collections().WaitPush(ctx(t), "s2", path, ids[nd.Name+path], nutest.Timeout)
			if err != nil || p.State != apiv0.PushWritten || p.Head == "" || p.Sources["visual.tif"].Size != int64(len(scene(i))) {
				t.Fatalf("%s %s: %+v %v", nd.Name, path, p, err)
			}
		}
	}
	if ranged.Load() == 0 {
		t.Fatal("the interrupted download did not resume with a range request")
	}
	nutest.Eventually(t, "one merged catalog on every member", func() bool {
		root := status(a).Root
		for _, nd := range []*nutest.Node{a, b, c} {
			if st := status(nd); st.Root != root || st.Items != items || st.Conflicts != 0 || len(st.Writers) != 2 {
				return false
			}
		}
		return root != ""
	})
	// Both receiving members hold every item as its writer (and report it so), whichever
	// writer the merged view credits with the identical entries.
	nutest.Eventually(t, "both receiving members hold every item", func() bool {
		return status(a).Local[collection.StateComplete] == items && status(b).Local[collection.StateComplete] == items
	})
	page, err := c.Local.CollectionItems(ctx(t), "s2", apiv0.CollectionItemsQuery{Since: "0", Limit: 1000})
	if err != nil || len(page.Items) != items {
		t.Fatalf("item stream on c: %d items, %v", len(page.Items), err)
	}
	log, err := a.Local.CollectionPushes(ctx(t), "s2", "0", 1000)
	if err != nil || len(log.Items) != items {
		t.Fatalf("push log on a: %+v %v", log, err)
	}
	if next, err := a.Local.CollectionPushes(ctx(t), "s2", log.NextCursor, 10); err != nil || len(next.Items) != 0 || next.NextCursor != log.NextCursor {
		t.Fatalf("push log after its cursor: %+v %v", next, err)
	}

	// Egress: an unlisted host is refused at accept, a redirect to it fails the push;
	// neither name is ever looked up.
	one := func(path, url, checksum string, replace bool) (*apiv0.CollectionPush, error) {
		p, err := a.Local.CollectionPush(ctx(t), "s2", apiv0.CollectionPushRequest{Path: path, Meta: []byte(`{"v": 1}`), Replace: replace,
			Files: map[string]apiv0.CollectionPushFile{"f": {URL: url, Checksum: checksum}}}, nutest.Timeout)
		return p, err
	}
	if _, err := one("bad/unlisted", "http://secret.test/x", "", false); apiCode(err) != apiv0.CodeForbidden {
		t.Fatalf("unlisted host: %v", err)
	}
	if p, err := one("bad/redirect", base+"/redirect", "", false); err != nil || p.State != apiv0.PushFailed || p.Error.Code != apiv0.CodeForbidden || p.Tries != 1 {
		t.Fatalf("redirect to an unlisted host: %+v %v", p, err)
	}
	if p, err := one("bad/missing", base+"/missing", "", false); err != nil || p.State != apiv0.PushFailed || p.Tries != 1 {
		t.Fatalf("404: %+v %v", p, err)
	}
	if p, err := one("bad/checksum", base+"/items/1/visual.tif", "1220"+strings.Repeat("00", 32), false); err != nil ||
		p.State != apiv0.PushFailed || !strings.Contains(p.Error.Message, "checksum") {
		t.Fatalf("checksum mismatch: %+v %v", p, err)
	}
	if eg.count("secret.test") != 0 {
		t.Fatalf("secret.test was looked up %d times", eg.count("secret.test"))
	}
	// Conflict and replace at a path only a writes, then removal.
	if p, err := one("solo/item", base+"/items/1/visual.tif", "", false); err != nil || p.State != apiv0.PushWritten {
		t.Fatalf("solo item: %+v %v", p, err)
	}
	if p, err := one("solo/item", base+"/items/2/visual.tif", "", false); err != nil || p.State != apiv0.PushFailed || p.Error.Code != apiv0.CodeConflict {
		t.Fatalf("different entry without replace: %+v %v", p, err)
	}
	if p, err := one("solo/item", base+"/items/2/visual.tif", "", true); err != nil || p.State != apiv0.PushWritten {
		t.Fatalf("replace: %+v %v", p, err)
	}
	if p, err := a.Local.CollectionRemove(ctx(t), "s2", "solo/item", nutest.Timeout); err != nil || p.State != apiv0.PushRemoved || p.Head == "" {
		t.Fatalf("remove: %+v %v", p, err)
	}
	nutest.Eventually(t, "removal in the catalog", func() bool {
		_, err := a.Local.CollectionItem(context.Background(), "s2", "solo/item")
		it, _ := a.Local.CollectionItem(context.Background(), "s2", "solo/item")
		return err == nil && it.Entry == "" && it.Push != nil && it.Push.State == apiv0.PushRemoved
	})
}

func apiCode(err error) apiv0.Code {
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// URL egress by network mode: isolated never fetches, private only ingest.hosts (none
// when unset), public any public host unless ingest.hosts narrows it.
func TestEgressHosts(t *testing.T) {
	listed := &cluster.Dataset{Source: cluster.Source{Local: true, Ingest: &cluster.Ingest{Hosts: []string{"scenes.test"}}}}
	bare := &cluster.Dataset{Source: cluster.Source{Local: true}}
	for _, c := range []struct {
		mode  string
		d     *cluster.Dataset
		hosts []string
		ok    bool
	}{
		{cluster.ModeIsolated, listed, nil, false},
		{cluster.ModePrivate, listed, []string{"scenes.test"}, true},
		{cluster.ModePrivate, bare, []string{}, true},
		{cluster.ModePublic, bare, nil, true},
		{cluster.ModePublic, listed, []string{"scenes.test"}, true},
	} {
		hosts, ok := collection.EgressHosts(c.mode, c.d)
		if ok != c.ok || (hosts == nil) != (c.hosts == nil) || strings.Join(hosts, ",") != strings.Join(c.hosts, ",") {
			t.Errorf("%s %+v: %v %v", c.mode, c.d.Source.Ingest, hosts, ok)
		}
	}
}
