package proto

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/store"
)

func TestPartialBatchVerifiedPrefix(t *testing.T) {
	const size = 6 * blob.GroupSize
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	ob := make(memAt, blob.OutboardSize(size))
	mh, err := blob.Outboard(ob, bytes.NewReader(data), size)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := blob.ExtractSlice(&encoded, bytes.NewReader(data), ob, size, blob.Range{End: 6}); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"corrupt", "truncated"} {
		t.Run(fault, func(t *testing.T) {
			st, err := store.Open(t.TempDir(), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			p, err := st.Partial(mh, size)
			if err != nil {
				t.Fatal(err)
			}
			body := bytes.Clone(encoded.Bytes())
			if fault == "corrupt" {
				body[len(body)-1] ^= 1
			} else {
				body = body[:len(body)-100]
			}
			err = readPartialSlice(bytes.NewReader(body), p, blob.Range{End: 6})
			if err == nil || (fault == "corrupt" && !errors.Is(err, ErrCorrupt)) {
				t.Fatal("fault not reported", err)
			}
			if !p.Has(blob.Range{End: 5}) || p.Has(blob.Range{Start: 5, End: 6}) {
				t.Fatal("verified prefix lost or bad group published", p.Held())
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			p, err = st.OpenPartial(mh)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			// Durable bitmap must refer to data AND proofs already written, not buffers.
			var got bytes.Buffer
			if err := p.ExtractSlice(&got, blob.Range{End: 5}); err != nil {
				t.Fatal(err)
			}
			if err := blob.DecodeSlice(&got, mh, size, blob.Range{End: 5}, blob.Sink{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingAt struct{ err error }

func (w *failingAt) WriteAt(p []byte, _ int64) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func TestPartialBatchWriteFailureDoesNotPublish(t *testing.T) {
	data := &failingAt{err: io.ErrClosedPipe}
	marked := 0
	b := partialBatch{
		dst:     blob.Sink{Group: func(uint64) { marked++ }},
		data:    pendingAt{dst: data, buf: make([]byte, 64)},
		parents: pendingAt{dst: &failingAt{}, buf: make([]byte, 64)},
	}
	if _, err := b.WriteAt([]byte("verified"), 0); err != nil {
		t.Fatal(err)
	}
	b.groups = append(b.groups, 0)
	if marked != 0 {
		t.Fatal("published before writing")
	}
	if err := b.flush(); !errors.Is(err, io.ErrClosedPipe) || marked != 0 {
		t.Fatal("published after failed write", err, marked)
	}
	data.err = nil
	if err := b.flush(); err != nil || marked != 1 {
		t.Fatal("retry", err, marked)
	}
}
