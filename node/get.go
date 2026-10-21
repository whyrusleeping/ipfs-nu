package node

import (
	"context"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/extract"
	"github.com/whyrusleeping/nu/node/link"
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
	root, err := parseCID(pt.CID)
	if err != nil {
		return nil, err
	}
	var hints []link.Provider
	if pt.Link != nil {
		hints = pt.Link.Providers
	}
	switch {
	case req.Output == "" && req.Dir == "":
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "get needs an output path or a directory to write into")
	case req.Output != "" && !filepath.IsAbs(req.Output), req.Output == "" && !filepath.IsAbs(req.Dir):
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "get needs an absolute output path").
			WithAction("the daemon writes the path itself, so it must not depend on the caller's working directory")
	}
	return n.start("get", pt.Raw, func(o *op.Operation) (any, error) {
		return n.get(o, root, pt.Path, hints, req)
	}), nil
}

func (n *Local) get(o *op.Operation, root cid.CID, p string, hints []link.Provider, req apiv0.GetRequest) (*apiv0.GetResult, error) {
	ctx := o.Context()
	res := &apiv0.GetResult{CID: root.String(), Path: p}
	fetch := func(c cid.CID) error {
		o.Stage("fetching")
		res.Fetched = true
		last := time.Now()
		return n.Fetcher.Fetch(ctx, c, hints, func(done, total int64) {
			if time.Since(last) > 100*time.Millisecond {
				o.Progress(done, total, "bytes")
				last = time.Now()
			}
		})
	}
	o.Stage("resolving")
	nd, err := n.content.Resolve(root, p)
	if _, notHeld := content.IsNotHeld(err); notHeld {
		// The root (or structure on the way to the path) is missing: fetch from the root.
		if err := fetch(root); err != nil {
			return nil, err
		}
		o.Stage("resolving")
		nd, err = n.content.Resolve(root, p)
	}
	if err != nil {
		return nil, err
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

// progress counts extracted bytes into the operation, rate-limited.
type progress struct {
	o           *op.Operation
	done, total int64
	last        time.Time
}

func (p *progress) add(n int) {
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
			if !n.walkNode(ctx, path.Join(p, child.Name), child, prog, yield) {
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
