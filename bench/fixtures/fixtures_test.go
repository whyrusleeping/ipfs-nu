package fixtures

import (
	"testing"

	_ "github.com/whyrusleeping/nu/nutest/testenv" // tmpfs temp dirs
)

// Fixtures are reproducible (same seed, same bytes; another seed differs), a materialized
// version sequence matches its listings on disk, and Change reports the intended shape:
// a monolithic rewrite changes the whole file but few novel bytes; a renamed log segment
// and a byte-identical dump part are not novel.
func TestFixtures(t *testing.T) {
	small := Params{Seed: 7, Scale: 0.05}
	for _, s := range Shapes {
		t.Run(s.Name, func(t *testing.T) {
			a, b := s.Versions(small, 4), s.Versions(small, 4)
			other := s.Versions(Params{Seed: 8, Scale: 0.05}, 1)
			if Digest(a[3]) != Digest(b[3]) {
				t.Fatal("same seed, different version 3")
			}
			if Digest(a[0]) == Digest(other[0]) {
				t.Fatal("another seed, same version 0")
			}
			dir := t.TempDir()
			var prev []File
			for v, files := range a {
				if err := Materialize(dir, prev, files); err != nil {
					t.Fatal(err)
				}
				if err := Verify(dir, files); err != nil {
					t.Fatalf("v%d: %v", v, err)
				}
				c := Diff(prev, files)
				if v > 0 && (c.WholeFileBytes() == 0 || c.NovelBytes == 0 || c.NovelBytes > c.WholeFileBytes()) {
					t.Fatalf("v%d change %+v", v, c)
				}
				prev = files
			}
		})
	}

	rw, _ := Lookup("rewrite")
	vs := rw.Versions(Params{Seed: 1, Scale: 0.25}, 2)
	c := Diff(vs[0], vs[1])
	if c.Modified != 1 || c.ModifiedBytes != 8<<20+64<<10 || c.NovelBytes > 16*4096+64<<10 || c.NovelBytes < 64<<10 {
		t.Fatalf("rewrite change %+v", c)
	}

	ap, _ := Lookup("append")
	vs = ap.Versions(Params{Seed: 1, Scale: 0.05}, 12)
	rotated := false
	for v := 1; v < len(vs); v++ {
		c := Diff(vs[v-1], vs[v])
		rotated = rotated || c.Added > 0
		if want := int64(4*(256<<10)/20 + 2<<10); c.NovelBytes != want {
			t.Fatalf("append v%d novel %d, want %d (%+v)", v, c.NovelBytes, want, c)
		}
	}
	if !rotated {
		t.Fatal("no log rotated")
	}

	dump, _ := Lookup("dump")
	vs = dump.Versions(Params{Seed: 1, Scale: 0.05}, 2)
	// Novel: the changed parts once (dumps/<date>/ and latest/ share them) plus the two
	// small metadata files; the unchanged quarter is not novel.
	if c := Diff(vs[0], vs[1]); c.Modified != 7 || c.NovelBytes != c.ModifiedBytes+3<<10 {
		t.Fatalf("dump change %+v", c)
	}
}
