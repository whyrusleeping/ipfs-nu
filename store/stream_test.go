package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/nutest/timeouts"
)

// A reader opened before anything arrives follows the partial as its groups verify (here
// last to first) and reads exactly the blob; a reader of a partial that closes incomplete
// once the fetch has returned fails with ErrNotArriving.
func TestOpenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	s := open(t, t.TempDir())

	size := uint64(8*blob.GroupSize + 100)
	src, _, mh, fetch := partialOf(t, size)
	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	go func() {
		r, err := s.OpenStream(ctx, mh, make(chan struct{}))
		if err == nil {
			var b []byte
			b, err = io.ReadAll(r)
			r.Close()
			got <- b
		}
		errc <- err
	}()
	p, err := s.Partial(mh, size)
	if err != nil {
		t.Fatal(err)
	}
	for g := blob.Groups(size); g > 0; g-- {
		fetch(p, blob.Range{Start: g - 1, End: g})
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(<-got, src) {
		t.Fatal("streamed bytes differ")
	}

	size = 4*blob.GroupSize + 1
	src, _, mh, fetch = partialOf(t, size)
	if p, err = s.Partial(mh, size); err != nil {
		t.Fatal(err)
	}
	fetch(p, blob.Range{Start: 0, End: 1})
	done := make(chan struct{})
	r, err := s.OpenStream(ctx, mh, done)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := make([]byte, blob.GroupSize)
	if _, err := io.ReadFull(r, first); err != nil || !bytes.Equal(first, src[:blob.GroupSize]) {
		t.Fatalf("first group: %v", err)
	}
	go func() { p.Close(); close(done) }()
	if _, err := io.ReadAll(r); !errors.Is(err, ErrNotArriving) {
		t.Fatalf("read past an abandoned partial: %v", err)
	}
}
