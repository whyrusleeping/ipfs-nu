package store

import (
	"sync"
	"testing"

	"github.com/whyrusleeping/nu/core/blob"
)

func TestPartialCheckpointRetry(t *testing.T) {
	const size = 8 * blob.GroupSize
	_, _, mh, receive := partialOf(t, size)
	s := open(t, t.TempDir())
	p, err := s.Partial(mh, size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p != nil {
			p.Close()
		}
	})
	receive(p, blob.Range{Start: 0, End: 1})
	// Fail after the fsyncs, at the durable bitmap update. No new groups arrive
	// before retry: an incorrectly cleared dirty bit would lose this group.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON partials BEGIN SELECT RAISE(ABORT, 'checkpoint fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(); err == nil {
		t.Fatal("injected checkpoint failure was ignored")
	}
	_, held, _, err := s.HeldRanges(mh)
	if err != nil || len(held) != 0 {
		t.Fatal("failed checkpoint published bitmap", held, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_checkpoint`); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	_, held, _, err = s.HeldRanges(mh)
	if err != nil || len(held) != 1 || held[0] != (blob.Range{Start: 0, End: 1}) {
		t.Fatal("retry lost verified group", held, err)
	}

	// Concurrent verified writes and checkpoints cannot regress the durable map
	// or reservation accounting. Closing and reopening resumes all eight groups.
	var wg sync.WaitGroup
	for g := uint64(1); g < 8; g++ {
		wg.Go(func() {
			receive(p, blob.Range{Start: g, End: g + 1})
			if err := p.Flush(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = s.Partial(mh, size)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Complete() || p.held != size || p.reserved != 0 {
		t.Fatalf("checkpoint regressed: missing=%v held=%d reserved=%d", p.Missing(), p.held, p.reserved)
	}
}
