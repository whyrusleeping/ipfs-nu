package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/store"
)

// N42 (0077 item 1): a published small file whose bytes are a MASL file node pointing at
// an unpublished blob must not be served under the DRISL codec of the file's multihash:
// the public set is keyed by full CID, and the store refuses a DRISL read of bytes that
// were stored raw.
func TestPublicCodecSwap(t *testing.T) {
	ctx := context.Background()
	n := newLocal(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.txt")
	os.WriteFile(secretPath, []byte("members only"), 0o644)
	sec, err := add(t, n, apiv0.AddRequest{Path: secretPath, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := n.content.Resolve(cid.MustParse(sec.CID), "")
	if err != nil {
		t.Fatal(err)
	}
	evil, err := drisl.Marshal(tree.FileNode{Src: sn.Blob, ContentType: "text/plain", Nu: tree.FileMeta{Size: uint64(sn.Size), Name: "x.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	os.WriteFile(filepath.Join(site, "evil.bin"), evil, 0o644)
	pub, err := add(t, n, apiv0.AddRequest{Path: site, Recursive: true, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	opts, err := n.EnablePublic(ctx, "seed.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: pub.CID}); err != nil {
		t.Fatal(err)
	}
	en, err := n.content.Resolve(cid.MustParse(pub.CID), "evil.bin")
	if err != nil {
		t.Fatal(err)
	}
	swapped := cid.New(cid.DRISL, en.Blob.Multihash())
	h := gateway.NewPublic(n.Content, opts)
	get := func(host, p string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://"+host+p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		b, _ := io.ReadAll(w.Result().Body)
		return w.Code, string(b)
	}
	// The published raw file itself is served.
	if code, body := get("seed.example", "/blob/"+en.Blob.String()); code != 200 || body != string(evil) {
		t.Fatalf("published blob: %d %q", code, body)
	}
	// Its multihash under the DRISL codec is not, on any route.
	for _, p := range []string{"/tree/" + swapped.String(), "/blob/" + swapped.String(), "/.well-known/rasl/" + swapped.String()} {
		if code, body := get("seed.example", p); code != 404 {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
	if code, body := get(swapped.String()+".seed.example", "/"); code != 404 {
		t.Fatalf("swapped site origin: %d %q", code, body)
	}
	// Nor is the private blob, and nothing reads the raw bytes as DRISL locally either.
	if code, _ := get("seed.example", "/blob/"+sn.Blob.String()); code != 404 {
		t.Fatalf("private blob: %d", code)
	}
	if _, err := n.Store.Get(swapped); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("store Get under the other codec: %v", err)
	}
	if _, err := n.content.Resolve(swapped, ""); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("loopback resolve of swapped CID: %v", err)
	}
}
