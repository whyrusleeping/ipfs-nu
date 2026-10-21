package nutest_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/nutest"
)

// 0028: add → share → get of a file and of a directory between nodes. The sharer runs a
// daemon (API + gateway); the recipients are a daemon and a node with no daemon and no
// prior init (a fresh repo, exactly what a daemonless `nu get` runs). The daemon's gateway
// then serves the fetched site on its <cid>.localhost origin, and a page on that origin
// cannot change daemon state through the API. (`get -o` safety against traversal and
// unsafe symlinks is node/extract's TestExtractSafety.)
func TestAddShareGet(t *testing.T) {
	n := nutest.New(t, 2, nutest.Options{API: true})
	sharer, browser := n.Nodes[0], n.Nodes[1]
	fresh := n.NewNode(false)

	site := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(filepath.Join(site, "sub"), 0o755)
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>hello</h1>"), 0o644)
	os.WriteFile(filepath.Join(site, "sub", "style.css"), []byte("body{}"), 0o644)
	dir := sharer.Add(site, true)
	file := sharer.AddBytes(n.RandBytes(100 << 10))

	ctx := context.Background()
	share := func(c string) string {
		t.Helper()
		res, err := sharer.Local.Share(ctx, apiv0.ShareRequest{Target: c})
		if err != nil || !res.Serving || len(res.Addrs) != 1 {
			t.Fatalf("share %s: %+v %v", c, res, err)
		}
		return res.Link
	}
	dirLink, fileLink := share(dir.String()), share(file.String())

	// A recipient with no daemon and no init gets both from the links alone.
	out := filepath.Join(t.TempDir(), "out")
	if res := fresh.Get(dirLink, filepath.Join(out, "site")); !res.Fetched || res.Files != 2 {
		t.Fatalf("fresh get dir: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(out, "site", "sub", "style.css")); err != nil || string(b) != "body{}" {
		t.Fatalf("fresh get dir content: %q %v", b, err)
	}
	if res := fresh.Get(fileLink, filepath.Join(out, "file")); !res.Fetched || res.Bytes != 100<<10 {
		t.Fatalf("fresh get file: %+v", res)
	}
	// A daemon fetches the site and serves it on its <cid>.localhost origin.
	browser.Get(dirLink, filepath.Join(out, "site2"))
	gw := browser.Daemon.GatewayAddr()
	_, gwPort, _ := strings.Cut(gw, ":")
	req, _ := http.NewRequest(http.MethodGet, "http://"+gw+"/sub/style.css", nil)
	req.Host = dir.String() + ".localhost:" + gwPort
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "body{}" {
		t.Fatalf("gateway on %s: %s %q", req.Host, res.Status, body)
	}
	// The page's origin cannot change daemon state through the API.
	api := browser.Daemon.APIAddr()
	req, _ = http.NewRequest(http.MethodDelete, "http://"+api+apiv0.Prefix+"/pins/"+dir.String(), nil)
	req.Header.Set("Origin", "http://"+dir.String()+".localhost:"+gwPort)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("API DELETE from the gateway origin: %s", res.Status)
	}
	if pins, err := browser.Local.Pins(ctx, "", 0); err != nil || len(pins.Items) != 0 {
		// get does not pin (the content is on disk); the point is that nothing changed.
		t.Fatalf("pins after refused request: %+v %v", pins, err)
	}
}
