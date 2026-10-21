package httpsrc_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/store"
)

// The existing reader-based store API needs no hook: the pipe propagates failure
// before EOF, which is essential to prevent committing a truncated URL import.
func TestStreamingStore(t *testing.T) {
	data := strings.Repeat("streaming", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // unknown length exercises the streaming size guard
		_, _ = io.WriteString(w, data)
	}))
	defer srv.Close()
	for _, max := range []int64{int64(len(data)), 100} {
		s, err := store.Open(t.TempDir(), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		opts := limits()
		opts.MaxBytes = max
		go func() {
			_, err := httpsrc.New(localConfig()).Fetch(context.Background(), srv.URL, func(int64) (io.Writer, error) { return pw, nil }, opts)
			_ = pw.CloseWithError(err)
			done <- err
		}()
		got, storeErr := s.AddBlob(pr, -1)
		_ = pr.CloseWithError(storeErr)
		fetchErr := <-done
		if max == int64(len(data)) {
			want := cid.New(cid.Raw, cid.Sum(cid.BLAKE3, []byte(data)))
			if storeErr != nil || fetchErr != nil || got != want {
				t.Fatalf("store %s: %v; fetch: %v", got, storeErr, fetchErr)
			}
		} else if !errors.Is(fetchErr, httpsrc.ErrTooLarge) || storeErr == nil {
			t.Fatalf("failed transfer committed: store %v; fetch %v", storeErr, fetchErr)
		}
	}
}
