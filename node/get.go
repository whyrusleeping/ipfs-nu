package node

import (
	"context"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/extract"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/node/target"
)

// maxSymlinkTarget bounds a symlink target read for the extraction report.
const maxSymlinkTarget = 4096

// Get validates the request and starts the get operation: stage `resolving` reads the
// target from the store, `fetching` asks the Fetcher for whatever is not held (with the
// link's provider hints), `writing` extracts it under the output path with byte
// progress. The result is an apiv0.GetResult.
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
	last := time.Now()
	nd, err := n.ensure(ctx, root, p, h, func(stage string) {
		o.Stage(stage)
		if stage == "fetching" {
			res.Fetched = true
		}
	}, func(done, total int64) {
		if time.Since(last) > 100*time.Millisecond {
			o.Progress(done, total, "bytes")
			last = time.Now()
		}
	})
	if err != nil {
		return nil, err
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

	o.Stage("writing")
	prog := &progress{o: o, total: nd.Size}
	rep, err := extract.Extract(ctx, out, n.walk(ctx, nd, prog))
	if err != nil {
		return nil, err
	}
	o.Progress(prog.done, prog.total, "bytes")
	res.Files, res.Dirs, res.Bytes = rep.Files, rep.Dirs, rep.Bytes
	for _, s := range rep.Skipped {
		res.Skipped = append(res.Skipped, s.Path+": "+s.Reason)
		o.Log("skipped " + s.Path + ": " + s.Reason)
	}
	return res, nil
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

// progress counts extracted bytes into the operation, rate-limited. Extraction writes
// files concurrently, so add takes a lock.
type progress struct {
	o           *op.Operation
	mu          sync.Mutex
	done, total int64
	last        time.Time
}

func (p *progress) add(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += int64(n)
	if time.Since(p.last) > 100*time.Millisecond {
		p.o.Progress(p.done, p.total, "bytes")
		p.last = time.Now()
	}
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
func (n *Local) walk(ctx context.Context, nd *content.Node, prog *progress) extract.Walk {
	return func(yield func(extract.Entry, error) bool) {
		n.walkNode(ctx, "", nd, prog, yield)
	}
}

func (n *Local) walkNode(ctx context.Context, p string, nd *content.Node, prog *progress, yield func(extract.Entry, error) bool) bool {
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
			if !n.walkNode(ctx, cp, child, prog, yield) {
				return false
			}
		}
		return true
	case gateway.KindSymlink:
		e.Kind = extract.KindSymlink
		f, err := n.content.Open(nd)
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
		e.Open = func(context.Context) (io.ReadCloser, error) {
			f, err := n.content.Open(nd)
			if err != nil {
				return nil, err
			}
			return countingReader{f, prog}, nil
		}
		return yield(e, nil)
	}
}
