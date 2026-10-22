package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestBatch: staged blobs are invisible until Commit, then held with their bytes; a blob
// staged twice commits once; Abort leaves no staging files behind (0084).
func TestBatch(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	data := make([]byte, 100<<10)
	rand.Read(data)
	other := make([]byte, 50<<10)
	rand.Read(other)

	b := s.Batch()
	c1, err := b.AddBlobHash(bytes.NewReader(data), int64(len(data)), s.opts.DefaultHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddBlobHash(bytes.NewReader(data), int64(len(data)), s.opts.DefaultHash); err != nil {
		t.Fatal(err)
	}
	if b.Len() != 2 || b.Bytes() <= int64(2*len(data)) {
		t.Fatalf("staged %d blobs, %d bytes", b.Len(), b.Bytes())
	}
	if _, ok, err := s.Has(c1); err != nil || ok {
		t.Fatalf("staged blob visible before Commit: %v %v", ok, err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if info, ok, err := s.Has(c1); err != nil || !ok || !info.Complete {
		t.Fatalf("committed blob not held: %+v %v %v", info, ok, err)
	}
	bl, err := s.OpenBlob(c1.Multihash())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	if _, err := bl.ReadAt(got, 0); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("committed bytes differ: %v", err)
	}
	bl.Close()

	c2, err := b.AddBlobHash(bytes.NewReader(other), int64(len(other)), s.opts.DefaultHash)
	if err != nil {
		t.Fatal(err)
	}
	b.Abort()
	if _, ok, _ := s.Has(c2); ok {
		t.Fatal("aborted blob is visible")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "staging")); len(ents) != 0 {
		t.Fatalf("staging not empty after Commit/Abort: %d entries", len(ents))
	}
}
