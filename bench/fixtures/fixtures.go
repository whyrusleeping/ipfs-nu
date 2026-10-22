// Package fixtures generates deterministic, seeded workload trees for the pilot
// benchmarks of backlog 0034 ([docs/pilots.md]). A Shape describes a workload (a small
// site, sparse small objects, a monolithic rewrite, appends, a sharded index, periodic
// dumps, a many-item collection); Versions lists its successive versions as file
// listings, and Materialize writes one to disk.
//
// These are synthetic stand-ins for real pilot workloads, not pilot data. Defaults are
// small (a few MiB to a few tens of MiB per version); Params.Scale grows them.
//
// Content is incompressible pseudo-random bytes addressed by (stream key, offset), so
// equal (key, offset) ranges are equal bytes wherever they appear and different keys never
// coincide. That lets the generator report, for each version, both the whole-file change
// cost and the bytes that are actually new (Change), against which a store's measured
// transfer and storage growth give an amplification factor.
package fixtures

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Seg is Len bytes of stream Key starting at stream offset Off.
type Seg struct {
	Key      uint64
	Off, Len int64
}

// File is one file of a version: its slash-separated relative path and content.
type File struct {
	Path string
	Segs []Seg
}

// Size is the file's length.
func (f File) Size() int64 {
	var n int64
	for _, s := range f.Segs {
		n += s.Len
	}
	return n
}

// Same reports whether two files have identical content descriptions (hence bytes).
func (f File) Same(g File) bool { return slices.Equal(f.Segs, g.Segs) }

// Params select a fixture instance.
type Params struct {
	// Seed drives every random choice (default 1).
	Seed int64
	// Scale multiplies the shape's dominant dimension: file or item counts for many-file
	// shapes, file sizes for monolithic ones (default 1; fractions shrink it).
	Scale float64
}

func (p Params) norm() Params {
	if p.Seed == 0 {
		p.Seed = 1
	}
	if p.Scale <= 0 {
		p.Scale = 1
	}
	return p
}

// Shape is a workload.
type Shape struct {
	Name string
	// Audience is "individual", "publisher" or "institutional".
	Audience string
	// Models says what real workload the shape stands for; Change what a version changes;
	// Measures what it is meant to expose.
	Models, Change, Measures string
	// Scales names the dimension Params.Scale multiplies.
	Scales string
	init   func(g *gen)
	step   func(g *gen, v int)
}

// Versions returns versions 0..n-1 of the shape: each a listing sorted by path.
func (s Shape) Versions(p Params, n int) [][]File {
	p = p.norm()
	h := fnv.New64a()
	h.Write([]byte(s.Name))
	g := &gen{p: p, rng: rand.New(rand.NewPCG(uint64(p.Seed), h.Sum64())), files: map[string]File{}}
	var out [][]File
	for v := range n {
		if v == 0 {
			s.init(g)
		} else {
			s.step(g, v)
		}
		out = append(out, g.snapshot())
	}
	return out
}

// Lookup returns the shape called name.
func Lookup(name string) (Shape, bool) {
	for _, s := range Shapes {
		if s.Name == name {
			return s, true
		}
	}
	return Shape{}, false
}

// Names lists the shapes' names.
func Names() []string {
	var out []string
	for _, s := range Shapes {
		out = append(out, s.Name)
	}
	return out
}

// gen is a shape's evolving state.
type gen struct {
	p     Params
	rng   *rand.Rand
	files map[string]File
	// state is per-shape bookkeeping (counters, log offsets).
	state map[string]int64
}

func (g *gen) key() uint64 { return g.rng.Uint64() }

// n scales a count (at least 1).
func (g *gen) n(base int) int { return max(1, int(math.Round(float64(base)*g.p.Scale))) }

// size scales a byte size (at least 1).
func (g *gen) size(base int64) int64 { return max(1, int64(math.Round(float64(base)*g.p.Scale))) }

// lognormal draws a size around median, clamped to [lo, hi].
func (g *gen) lognormal(median, lo, hi int64, sigma float64) int64 {
	v := int64(math.Exp(math.Log(float64(median)) + sigma*g.rng.NormFloat64()))
	return min(max(v, lo), hi)
}

// put sets path to fresh content of n bytes.
func (g *gen) put(path string, n int64) {
	g.files[path] = File{Path: path, Segs: []Seg{{Key: g.key(), Len: n}}}
}

func (g *gen) del(path string) { delete(g.files, path) }

// paths returns the current paths with prefix, sorted.
func (g *gen) paths(prefix string) []string {
	var out []string
	for p := range g.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// pick returns k distinct random elements of xs (all of them when k ≥ len).
func (g *gen) pick(xs []string, k int) []string {
	xs = slices.Clone(xs)
	g.rng.Shuffle(len(xs), func(i, j int) { xs[i], xs[j] = xs[j], xs[i] })
	return xs[:min(k, len(xs))]
}

func (g *gen) counter(name string) int64 {
	if g.state == nil {
		g.state = map[string]int64{}
	}
	g.state[name]++
	return g.state[name]
}

// overwrite replaces [off, off+n) of path with fresh bytes (an in-place write).
func (g *gen) overwrite(path string, off, n int64) {
	f := g.files[path]
	left, rest := split(f.Segs, off)
	_, right := split(rest, n)
	f.Segs = merge(append(append(left, Seg{Key: g.key(), Len: n}), right...))
	g.files[path] = f
}

// insert inserts n fresh bytes at off (a shifting edit).
func (g *gen) insert(path string, off, n int64) {
	f := g.files[path]
	left, right := split(f.Segs, off)
	f.Segs = merge(append(append(left, Seg{Key: g.key(), Len: n}), right...))
	g.files[path] = f
}

// appendTo appends n bytes continuing path's own stream (creating it if missing).
func (g *gen) appendTo(path string, n int64) {
	f, ok := g.files[path]
	if !ok {
		f = File{Path: path}
	}
	k := "stream:" + path
	if len(f.Segs) == 0 { // a new (or rotated) file starts a new stream
		g.counter(k)
		g.state[k] = int64(g.key() >> 1)
		g.state[k+":off"] = 0
	}
	key, off := uint64(g.state[k]), g.state[k+":off"]
	f.Segs = merge(append(slices.Clone(f.Segs), Seg{Key: key, Off: off, Len: n}))
	g.state[k+":off"] = off + n
	g.files[path] = f
}

func (g *gen) rename(from, to string) {
	f := g.files[from]
	delete(g.files, from)
	f.Path = to
	g.files[to] = f
}

func (g *gen) snapshot() []File {
	out := make([]File, 0, len(g.files))
	for _, f := range g.files {
		f.Segs = slices.Clone(f.Segs)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// split cuts segs at byte offset off.
func split(segs []Seg, off int64) (left, right []Seg) {
	var pos int64
	for i, s := range segs {
		switch {
		case pos+s.Len <= off:
			left = append(left, s)
		case pos >= off:
			return left, append(right, segs[i:]...)
		default:
			cut := off - pos
			left = append(left, Seg{s.Key, s.Off, cut})
			right = append(right, Seg{s.Key, s.Off + cut, s.Len - cut})
			return left, append(right, segs[i+1:]...)
		}
		pos += s.Len
	}
	return left, right
}

// merge joins adjacent contiguous segments of one stream and drops empty ones.
func merge(segs []Seg) []Seg {
	var out []Seg
	for _, s := range segs {
		if s.Len == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Key == s.Key && out[n-1].Off+out[n-1].Len == s.Off {
			out[n-1].Len += s.Len
			continue
		}
		out = append(out, s)
	}
	return out
}

// Change summarizes one version against the previous one.
type Change struct {
	Files      int   `json:"files"`
	Bytes      int64 `json:"bytes"`
	Added      int   `json:"added"`
	Modified   int   `json:"modified"`
	Removed    int   `json:"removed"`
	AddedBytes int64 `json:"added_bytes"`
	// ModifiedBytes is the full size of every modified file: what a store that keeps
	// whole files must take in for them.
	ModifiedBytes int64 `json:"modified_bytes"`
	RemovedBytes  int64 `json:"removed_bytes"`
	// NovelBytes is the bytes of this version that appear nowhere in the previous one
	// (by content, across files): the floor any deduplicating transfer must move.
	NovelBytes int64 `json:"novel_bytes"`
}

// WholeFileBytes is what changed counting whole files: added plus modified bytes.
func (c Change) WholeFileBytes() int64 { return c.AddedBytes + c.ModifiedBytes }

// Diff compares next with prev (nil for a first version).
func Diff(prev, next []File) Change {
	var c Change
	old := map[string]File{}
	for _, f := range prev {
		old[f.Path] = f
	}
	seen := Ranges{}
	seen.Add(prev)
	for _, f := range next {
		c.Files++
		c.Bytes += f.Size()
		if o, ok := old[f.Path]; !ok {
			c.Added++
			c.AddedBytes += f.Size()
		} else if !o.Same(f) {
			c.Modified++
			c.ModifiedBytes += f.Size()
		}
		delete(old, f.Path)
	}
	for _, f := range old {
		c.Removed++
		c.RemovedBytes += f.Size()
	}
	c.NovelBytes = seen.Novel(next)
	return c
}

// Ranges records which stream ranges have been seen (to count novel bytes over a run).
type Ranges map[uint64][][2]int64

// Add records every range of files.
func (r Ranges) Add(files []File) {
	for _, f := range files {
		for _, s := range f.Segs {
			r.add(s.Key, s.Off, s.Off+s.Len)
		}
	}
}

func (r Ranges) add(key uint64, lo, hi int64) {
	ivs := append(r[key], [2]int64{lo, hi})
	sort.Slice(ivs, func(i, j int) bool { return ivs[i][0] < ivs[j][0] })
	out := ivs[:1]
	for _, iv := range ivs[1:] {
		last := &out[len(out)-1]
		if iv[0] <= last[1] {
			last[1] = max(last[1], iv[1])
		} else {
			out = append(out, iv)
		}
	}
	r[key] = out
}

// Novel counts the bytes of files not in r (counting a range repeated within files once).
func (r Ranges) Novel(files []File) int64 {
	cur := Ranges{}
	var n int64
	for _, f := range files {
		for _, s := range f.Segs {
			lo, hi := s.Off, s.Off+s.Len
			n += (hi - lo) - r.covered(s.Key, lo, hi) - cur.coveredExcluding(r, s.Key, lo, hi)
			cur.add(s.Key, lo, hi)
		}
	}
	return n
}

func (r Ranges) covered(key uint64, lo, hi int64) int64 {
	var n int64
	for _, iv := range r[key] {
		a, b := max(lo, iv[0]), min(hi, iv[1])
		if a < b {
			n += b - a
		}
	}
	return n
}

// coveredExcluding counts bytes of [lo,hi) in r that are not also in seen.
func (r Ranges) coveredExcluding(seen Ranges, key uint64, lo, hi int64) int64 {
	var n int64
	for _, iv := range r[key] {
		a, b := max(lo, iv[0]), min(hi, iv[1])
		if a < b {
			n += (b - a) - seen.covered(key, a, b)
		}
	}
	return n
}

// Total sums the sizes of files.
func Total(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size()
	}
	return n
}

const blockSize = 4096

// block fills dst (blockSize bytes) with block idx of stream key.
func block(dst []byte, key uint64, idx int64) {
	r := rand.NewPCG(key, uint64(idx)*0x9e3779b97f4a7c15+1)
	for i := 0; i < len(dst); i += 8 {
		binary.LittleEndian.PutUint64(dst[i:], r.Uint64())
	}
}

// WriteContent writes f's bytes to w.
func WriteContent(w io.Writer, f File) error {
	buf := make([]byte, blockSize)
	for _, s := range f.Segs {
		for off, end := s.Off, s.Off+s.Len; off < end; {
			idx := off / blockSize
			block(buf, s.Key, idx)
			lo := off - idx*blockSize
			hi := min(int64(blockSize), end-idx*blockSize)
			if _, err := w.Write(buf[lo:hi]); err != nil {
				return err
			}
			off += hi - lo
		}
	}
	return nil
}

// Materialize turns dir, which holds the version prev (nil: dir is empty or missing),
// into next: writing new and changed files and removing the rest (and directories left
// empty). Unchanged files are not touched.
func Materialize(dir string, prev, next []File) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old := map[string]File{}
	for _, f := range prev {
		old[f.Path] = f
	}
	for _, f := range next {
		if o, ok := old[f.Path]; ok && o.Same(f) {
			delete(old, f.Path)
			continue
		}
		delete(old, f.Path)
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(f.Path)), f); err != nil {
			return err
		}
	}
	var gone []string
	for p := range old {
		gone = append(gone, p)
	}
	sort.Strings(gone)
	for _, p := range gone {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil {
			return err
		}
		for d := filepath.Dir(full); d != filepath.Clean(dir); d = filepath.Dir(d) {
			if os.Remove(d) != nil { // not empty
				break
			}
		}
	}
	return nil
}

func writeFile(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 256<<10)
	if err := WriteContent(w, f); err != nil {
		out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Digest is a SHA-256 over a listing's paths and bytes (a determinism check).
func Digest(files []File) [32]byte {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Path, f.Size())
		_ = WriteContent(h, f)
	}
	var d [32]byte
	h.Sum(d[:0])
	return d
}

// Verify checks that dir holds exactly files (paths, sizes and bytes).
func Verify(dir string, files []File) error {
	want := map[string]File{}
	for _, f := range files {
		want[f.Path] = f
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, ok := want[rel]
		if !ok {
			return fmt.Errorf("unexpected file %s", rel)
		}
		delete(want, rel)
		got, err := os.Open(p)
		if err != nil {
			return err
		}
		defer got.Close()
		hg, hw := sha256.New(), sha256.New()
		n, err := io.Copy(hg, got)
		if err != nil {
			return err
		}
		if n != f.Size() {
			return fmt.Errorf("%s: %d bytes, want %d", rel, n, f.Size())
		}
		_ = WriteContent(hw, f)
		if string(hg.Sum(nil)) != string(hw.Sum(nil)) {
			return fmt.Errorf("%s: content differs", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range want {
		return fmt.Errorf("missing file %s", p)
	}
	return nil
}
