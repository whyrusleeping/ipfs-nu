package httpsrc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/store"
)

// An interrupted download resumes with Range/If-Range on the strong ETag, across two
// Import calls sharing a partial file (a restart), and stores exactly the source bytes;
// a wrong checksum fails the import and demotes the origin.
func TestImportResumeAndChecksum(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	var requests, ranged atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
			if r.Header.Get("If-Range") != `"v1"` {
				t.Errorf("If-Range %q", r.Header.Get("If-Range"))
			}
		}
		if n == 1 {
			// Declare the whole body, send a third, then drop the connection.
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:len(data)/3])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := httpsrc.New(localConfig())
	o := httpsrc.ImportOptions{Options: httpsrc.Options{MaxBytes: 4 << 20, Timeout: 5 * time.Second, IdleTimeout: 5 * time.Second},
		Partial: filepath.Join(t.TempDir(), "partial")}
	if _, err := c.Import(context.Background(), srv.URL+"/scene.tif", st, o); err == nil {
		t.Fatal("first import succeeded despite the dropped connection")
	}
	sum := sha256.Sum256(data)
	ck, err := httpsrc.ParseChecksum("1220" + hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	o.Checksum = &ck
	got, err := c.Import(context.Background(), srv.URL+"/scene.tif", st, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := cid.New(cid.Raw, cid.Sum(cid.BLAKE3, data)); got.CID != want || !got.Resumed || ranged.Load() != 1 || got.Size != int64(len(data)) || got.ETag != `"v1"` {
		t.Fatalf("import %+v (want %s), ranged requests %d", got, want, ranged.Load())
	}

	bad := ck
	bad.Digest = append([]byte(nil), ck.Digest...)
	bad.Digest[0] ^= 1
	o.Checksum, o.Partial = &bad, ""
	if _, err := c.Import(context.Background(), srv.URL+"/scene.tif", st, o); !errors.Is(err, httpsrc.ErrChecksum) || !httpsrc.Permanent(err) {
		t.Fatalf("checksum mismatch: %v", err)
	}
	if !c.Demoted(srv.URL + "/other") {
		t.Fatal("origin not demoted after a checksum mismatch")
	}
	if _, err := httpsrc.ParseChecksum("d50110" + "00112233445566778899aabbccddeeff"); err == nil {
		t.Fatal("md5 checksum accepted")
	}
}
