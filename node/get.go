package node

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/extract"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/store"
)

// maxSymlinkTarget bounds a symlink target read for the extraction report.
const maxSymlinkTarget = 4096

// PartialSuffix names a get's output while it is written: a new output path is
// extracted to path+PartialSuffix and renamed into place once complete, so the final
// name only ever holds a whole result. A failed get leaves the partial; the next get of
// the same output replaces it (the fetched content stays in the store).
const PartialSuffix = ".nu-partial"

// Get validates the request and starts the get operation: stage `resolving` reads the
// target from the store, `fetching` asks the Fetcher for whatever is not held (with the
// link's provider hints) while files are already written as their bytes arrive, and
// `writing` finishes the extraction with byte progress. The result is an
// apiv0.GetResult.
func (n *Local) Get(ctx context.Context, req apiv0.GetRequest) (*apiv0.Operation, error) {
	pt, err := target.Parse(req.Target)
	if err != nil {
		return nil, err
	}
	if err := pt.Supported(); err != nil {
		return nil, err
	}
	_, root, err := n.parseTarget(ctx, req.Target) // also the at:// explanation
	if err != nil {
		return nil, err
	}
	opTarget := pt.Raw
	if pt.Kind == target.KindDataset || pt.Kind == target.KindName { // bind the operation to the resolved root (W1, E1)
		opTarget = root.String()
		if pt.Path != "" {
			opTarget += "/" + pt.Path
		}
	}
	h := targetHints(pt)
	switch {
	case req.Output == "" && req.Dir == "":
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "get needs an output path or a directory to write into")
	case req.Output != "" && !filepath.IsAbs(req.Output), req.Output == "" && !filepath.IsAbs(req.Dir):
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "get needs an absolute output path").
			WithAction("the daemon writes the path itself, so it must not depend on the caller's working directory")
	}
	return n.start("get", opTarget, func(o *op.Operation) (any, error) {
		return n.get(o, root, pt.Path, h, req)
	}), nil
}

func (n *Local) get(o *op.Operation, root cid.CID, p string, h hints, req apiv0.GetRequest) (*apiv0.GetResult, error) {
	ctx := o.Context()
	res := &apiv0.GetResult{CID: root.String(), Path: p}
	prog := &progress{o: o}
	// The fetch runs in the background once needed; extraction starts as soon as the
	// structure is held and writes each file as its bytes arrive.
	var bg *background
	defer func() {
		if bg != nil {
			bg.stop()
		}
	}()
	fetch := func(c cid.CID) error {
		res.Fetched = true
		prog.quiet.Store(true)
		bg = n.fetchInBackground(ctx, c, h, o, prog)
		return bg.waitStructure(ctx)
	}
	o.Stage("resolving")
	nd, err := n.content.Resolve(root, p)
	if _, notHeld := content.IsNotHeld(err); notHeld {
		// The root (or structure on the way to the path) is missing: fetch from the root.
		if err := fetch(root); err != nil {
			return nil, err
		}
		if nd, err = n.content.Resolve(root, p); err != nil {
			if _, notHeld := content.IsNotHeld(err); notHeld {
				if err := bg.wait(); err != nil {
					return nil, err
				}
				nd, err = n.content.Resolve(root, p)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if bg == nil {
		// Structure resolved; make sure everything under the resolved node is here.
		if err := n.requireComplete(ctx, nd.CID); err != nil {
			if _, notHeld := content.IsNotHeld(err); !notHeld {
				return nil, err
			}
			if err := fetch(nd.CID); err != nil {
				return nil, err
			}
		}
	}
	out := req.Output
	if out == "" {
		out = filepath.Join(req.Dir, defaultName(nd, root, p))
	}
	res.Output, res.Kind = out, string(nd.Kind)
	if nd.Kind == gateway.KindSymlink {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a symlink; symlinks are not extracted", req.Target).
			WithAction("get the directory that contains it, or the file it points to")
	}

	// A new output is written under a partial name while the fetch runs and renamed when
	// complete. An existing one is updated in place, file by file, only once the fetch
	// has succeeded, so a failed get never leaves it half updated.
	dest, partial := out, ""
	prog.total.Store(nd.Size)
	if _, err := os.Lstat(out); errors.Is(err, fs.ErrNotExist) {
		partial = out + PartialSuffix
		if err := os.RemoveAll(partial); err != nil {
			return nil, err
		}
		dest = partial
	} else if bg != nil {
		if err := bg.wait(); err != nil {
			return nil, err
		}
	}
	ectx, arriving := ctx, (<-chan struct{})(nothingArriving)
	if bg != nil {
		ectx, arriving = bg.extract, bg.done
	} else {
		o.Stage("writing")
	}
	rep, err := extract.Extract(ectx, dest, n.walk(ectx, nd, prog, arriving))
	if bg != nil {
		if err != nil && !bg.finished() {
			bg.stop() // the extraction failed on its own: stop fetching
		} else if ferr := bg.wait(); ferr != nil {
			err = ferr // the fetch's failure explains the extraction's
		}
	}
	if err != nil {
		if partial != "" {
			if _, serr := os.Lstat(partial); serr == nil {
				o.Log("the partial result is in " + partial + "; get again to finish it (fetched content is kept)")
			}
		}
		return nil, err
	}
	if partial != "" {
		if _, err := os.Lstat(out); err == nil {
			return nil, apiv0.Errorf(apiv0.CodeConflict, "%s appeared while the get ran; the result is in %s", out, partial).
				WithAction("move one of them aside")
		}
		if err := os.Rename(partial, out); err != nil {
			return nil, err
		}
	}
	prog.quiet.Store(false)
	o.Progress(prog.done.Load(), prog.total.Load(), "bytes")
	res.Files, res.Dirs, res.Bytes = rep.Files, rep.Dirs, rep.Bytes
	for _, s := range rep.Skipped {
		res.Skipped = append(res.Skipped, s.Path+": "+s.Reason)
		o.Log("skipped " + s.Path + ": " + s.Reason)
	}
	return res, nil
}

// nothingArriving is an always-closed channel: no fetch is running.
var nothingArriving = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// background is a get's fetch running alongside the extraction.
type background struct {
	cancel    context.CancelFunc // stops the fetch and the extraction
	done      chan struct{}      // closed when the fetch returned
	err       error              // the fetch's result, once done
	structure chan struct{}      // closed once the structure is held (or the fetch returned)
	extract   context.Context    // the extraction's context, cancelled if the fetch fails
}

// fetchInBackground starts fetching c. Fetcher progress begins once the structure is
// held (the Fetcher contract), which is when the extraction can walk the tree. When the
// fetch succeeds the stage becomes `writing` and the extraction's progress is reported.
func (n *Local) fetchInBackground(ctx context.Context, c cid.CID, h hints, o *op.Operation, prog *progress) *background {
	fctx, cancel := context.WithCancel(ctx)
	ectx, ecancel := context.WithCancel(ctx)
	b := &background{cancel: func() { cancel(); ecancel() }, done: make(chan struct{}), structure: make(chan struct{}), extract: ectx}
	var once sync.Once
	ready := func() { once.Do(func() { close(b.structure) }) }
	o.Stage("fetching")
	var last time.Time
	go func() {
		b.err = n.fetch(fctx, c, h, func(done, total int64) {
			ready()
			if time.Since(last) > 100*time.Millisecond {
				o.Progress(done, total, "bytes")
				last = time.Now()
			}
		})
		close(b.done) // before structure, so waitStructure sees a failure
		ready()
		if b.err != nil {
			ecancel()
			return
		}
		o.Stage("writing")
		prog.quiet.Store(false)
		prog.mu.Lock()
		prog.report()
		prog.mu.Unlock()
	}()
	return b
}

// waitStructure waits until the extraction can walk the structure; a fetch that failed
// before reports its error.
func (b *background) waitStructure(ctx context.Context) error {
	select {
	case <-b.structure:
	case <-ctx.Done():
		return ctx.Err()
	}
	if b.finished() {
		return b.err
	}
	return nil
}

func (b *background) finished() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *background) wait() error {
	<-b.done
	return b.err
}

func (b *background) stop() {
	b.cancel()
	<-b.done
}

// ensure resolves root/p and makes everything under the resolved node held, fetching
// through the Fetcher (with the target's peer and RASL hints) when the root, the
// structure on the way to p, or anything under the resolved node is missing. stage and
// progress may be nil.
func (n *Local) ensure(ctx context.Context, root cid.CID, p string, h hints, stage func(string), progress func(done, total int64)) (*content.Node, error) {
	return n.ensureWith(ctx, root, p, h, stage, progress, false)
}

// ensureWith is ensure; with listing set, a resolved directory needs only its own
// structure (a listing), not the content below it, so a partially held tree (a select
// follow's catalog) lists without fetching its items.
func (n *Local) ensureWith(ctx context.Context, root cid.CID, p string, h hints, stage func(string), progress func(done, total int64), listing bool) (*content.Node, error) {
	if stage == nil {
		stage = func(string) {}
	}
	fetch := func(c cid.CID) error {
		stage("fetching")
		return n.fetch(ctx, c, h, progress)
	}
	stage("resolving")
	nd, err := n.content.Resolve(root, p)
	if _, notHeld := content.IsNotHeld(err); notHeld {
		// The root (or structure on the way to the path) is missing: fetch from the root.
		if err := fetch(root); err != nil {
			return nil, err
		}
		stage("resolving")
		nd, err = n.content.Resolve(root, p)
	}
	if err != nil {
		return nil, err
	}
	if listing && nd.Kind == gateway.KindDir {
		return nd, nil
	}
	// Structure resolved; make sure everything under the resolved node is here.
	if err := n.requireComplete(ctx, nd.CID); err != nil {
		if _, notHeld := content.IsNotHeld(err); !notHeld {
			return nil, err
		}
		if err := fetch(nd.CID); err != nil {
			return nil, err
		}
	}
	return nd, nil
}

// defaultName names the output when the caller gave a directory: the path's last segment,
// the file node's name, else the CID.
func defaultName(nd *content.Node, root cid.CID, p string) string {
	if p != "" {
		return path.Base(p)
	}
	if nd.Kind == gateway.KindFile && nd.Name != "" && !strings.ContainsAny(nd.Name, "/\\\x00") && nd.Name != "." && nd.Name != ".." {
		return nd.Name
	}
	return root.String()
}

// progress counts extracted bytes into the operation, rate-limited. It stays quiet while
// a fetch reports its own progress. Extraction writes files concurrently.
type progress struct {
	o     *op.Operation
	total atomic.Int64
	done  atomic.Int64
	quiet atomic.Bool
	mu    sync.Mutex
	last  time.Time
}

func (p *progress) add(n int) {
	p.done.Add(int64(n))
	if p.quiet.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.last) > 100*time.Millisecond {
		p.report()
	}
}

// report sends the extracted bytes so far; callers hold mu and rate-limit.
func (p *progress) report() {
	p.o.Progress(p.done.Load(), p.total.Load(), "bytes")
	p.last = time.Now()
}

type countingReader struct {
	io.ReadCloser
	p *progress
}

func (r countingReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.p.add(n)
	return n, err
}

// walk yields the resolved node and, for a directory, everything under it in pre-order
// as extract entries. A blob root is extracted as a file.
// File bytes are opened with store.OpenStream: arriving stays open while a fetch may
// still deliver them.
func (n *Local) walk(ctx context.Context, nd *content.Node, prog *progress, arriving <-chan struct{}) extract.Walk {
	w := &walker{n: n, prog: prog, arriving: arriving}
	return func(yield func(extract.Entry, error) bool) {
		w.node(ctx, "", nd, yield)
	}
}

type walker struct {
	n        *Local
	prog     *progress
	arriving <-chan struct{}
}

// open opens a file or symlink node's bytes, waiting for them while they arrive.
func (w *walker) open(ctx context.Context, nd *content.Node) (io.ReadCloser, error) {
	if !nd.Blob.Defined() {
		return w.n.content.Open(nd) // its error
	}
	r, err := w.n.Store.OpenStream(ctx, nd.Blob.Multihash(), w.arriving)
	if errors.Is(err, store.ErrNotArriving) {
		return nil, content.NotHeld(nd.Blob)
	}
	return r, err
}

func (w *walker) node(ctx context.Context, p string, nd *content.Node, yield func(extract.Entry, error) bool) bool {
	n := w.n
	if err := ctx.Err(); err != nil {
		return yield(extract.Entry{}, err)
	}
	e := extract.Entry{Path: p, Mode: nd.Mode, Size: nd.Size}
	switch nd.Kind {
	case gateway.KindDir:
		e.Kind = extract.KindDir
		if !yield(e, nil) {
			return false
		}
		for child, err := range n.content.Entries(nd) {
			if err != nil {
				return yield(extract.Entry{}, err)
			}
			// Joined unclean, so extract.Extract's X1 check sees the name as stored.
			cp := child.Name
			if p != "" {
				cp = p + "/" + child.Name
			}
			if !w.node(ctx, cp, child, yield) {
				return false
			}
		}
		return true
	case gateway.KindSymlink:
		e.Kind = extract.KindSymlink
		f, err := w.open(ctx, nd)
		if err != nil {
			return yield(extract.Entry{}, err)
		}
		b, err := io.ReadAll(io.LimitReader(f, maxSymlinkTarget))
		f.Close()
		if err != nil {
			return yield(extract.Entry{}, err)
		}
		e.Target = string(b)
		return yield(e, nil)
	default: // file or blob
		e.Kind = extract.KindFile
		e.Open = func(ctx context.Context) (io.ReadCloser, error) {
			f, err := w.open(ctx, nd)
			if err != nil {
				return nil, err
			}
			return countingReader{f, w.prog}, nil
		}
		return yield(e, nil)
	}
}
