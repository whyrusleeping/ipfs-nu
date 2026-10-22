// Package content resolves held content for the node and the gateway: a root CID with an
// optional slash path becomes a tree directory, a file in a tree, a single-file MASL node,
// a symlink entry or a raw blob, read from the store. It implements gateway.Content.
package content

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"path"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/store"
)

// Content reads trees, file nodes and blobs from a store.
type Content struct {
	st       *store.Store
	datasets func(ctx context.Context, name, version string) (string, error)
	names    func(ctx context.Context, authority, name string, held bool) (string, string, error)
}

// SetNames installs the public-name lookup the gateway's /nu/ route uses (0045).
func (c *Content) SetNames(fn func(ctx context.Context, authority, name string, held bool) (string, string, error)) {
	c.names = fn
}

// ResolveName implements gateway.NameContent through the node's lookup.
func (g gw) ResolveName(ctx context.Context, authority, name string, held bool) (string, string, error) {
	if g.c.names == nil {
		return "", "", apiv0.NotImplemented("names on the gateway")
	}
	return g.c.names(ctx, authority, name, held)
}

// SetDatasets installs the dataset lookup the gateway's /c/ route uses (the node sets it).
func (c *Content) SetDatasets(fn func(ctx context.Context, name, version string) (string, error)) {
	c.datasets = fn
}

// New returns content served from st.
func New(st *store.Store) *Content { return &Content{st: st} }

// ResolveDataset implements gateway.DatasetContent through the node's lookup.
func (g gw) ResolveDataset(ctx context.Context, name, version string) (string, error) {
	if g.c.datasets == nil {
		return "", apiv0.NotImplemented("datasets on the gateway")
	}
	return g.c.datasets(ctx, name, version)
}

// Node is a resolved object.
type Node struct {
	Kind gateway.Kind
	// CID is the resolved node: the raw blob of a file in a tree, the MASL node of a
	// single-file root, the header or MST node of a directory.
	CID cid.CID
	// Blob is the raw blob of a file, blob or symlink.
	Blob cid.CID
	// Dir is the MST root of a directory.
	Dir  cid.CID
	Name string
	Size int64
	Mime string
	Mode uint32
}

// MissingDetail is the error detail key naming the CID whose content is not (fully)
// held; IsNotHeld reads it.
const MissingDetail = "missing"

const notHeldAction = "add the content locally, or get it from a holder with `get <link>`"

// NotHeld is the error for content the store does not have.
func NotHeld(c cid.CID) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeNotFound, "%s is not held by this node", c).
		WithAction(notHeldAction).WithDetail(MissingDetail, c.String())
}

// Incomplete is the error for content whose structure is held only in part.
func Incomplete(c cid.CID, missing, objects int) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeNotFound, "%d of %d objects under %s are not held by this node", missing, objects, c).
		WithAction(notHeldAction).WithDetail(MissingDetail, c.String())
}

// IsNotHeld reports whether err says content is missing from the store (as opposed to a
// path that does not exist in held content), returning the CID to fetch.
func IsNotHeld(err error) (cid.CID, bool) {
	var ae *apiv0.Error
	if !errors.As(err, &ae) || ae.Code != apiv0.CodeNotFound {
		return cid.CID{}, false
	}
	s, _ := ae.Details[MissingDetail].(string)
	c, perr := cid.Parse(s)
	return c, perr == nil
}

func notFoundPath(root cid.CID, p string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return apiv0.Errorf(apiv0.CodeNotFound, "part of %s is not held by this node", root).
			WithAction(notHeldAction).WithDetail(MissingDetail, root.String())
	}
	if errors.Is(err, tree.ErrNotFound) {
		return apiv0.Errorf(apiv0.CodeNotFound, "no %q in %s", p, root).WithAction("list the directory with `ls`")
	}
	return err
}

// Resolve follows p (slash-separated, may be empty) from root.
func (c *Content) Resolve(root cid.CID, p string) (*Node, error) {
	p = strings.Trim(path.Clean("/"+p), "/")
	if root.Codec() == cid.Raw {
		info, ok, err := c.st.Has(root)
		if err != nil {
			return nil, err
		}
		if !ok || !info.Complete {
			return nil, NotHeld(root)
		}
		if p != "" {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is a blob; it has no %q inside", root, p)
		}
		return &Node{Kind: gateway.KindBlob, CID: root, Blob: root, Size: int64(info.Size)}, nil
	}
	data, err := c.st.Get(root)
	if errors.Is(err, store.ErrNotFound) {
		return nil, NotHeld(root)
	}
	if err != nil {
		return nil, err
	}
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", root, err)
	}
	m, _ := v.(map[string]any)
	var dir cid.CID
	var size int64
	switch m["$type"] {
	case tree.TypeHeader:
		h, err := tree.DecodeHeader(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", root, err)
		}
		dir, size = h.Root, int64(h.Bytes)
	case tree.TypeMASL:
		f, err := tree.DecodeFileNode(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", root, err)
		}
		if p != "" {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is a file; it has no %q inside", root, p)
		}
		return &Node{Kind: gateway.KindFile, CID: root, Blob: f.Src, Name: f.Nu.Name, Size: int64(f.Nu.Size),
			Mime: f.ContentType, Mode: f.Nu.Mode}, nil
	default:
		if _, ok := m["e"]; !ok {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a DRISL object, not a tree, file or blob", root).
				WithAction("read it with `dag get`")
		}
		dir = root
	}
	if p == "" {
		return &Node{Kind: gateway.KindDir, CID: root, Dir: dir, Size: size}, nil
	}
	e, err := tree.Resolve(c.st, dir, p)
	if err != nil {
		return nil, notFoundPath(root, p, err)
	}
	return entryNode(path.Base(p), e), nil
}

func entryNode(name string, e tree.Entry) *Node {
	n := &Node{CID: e.CID, Name: name, Size: int64(e.Size), Mime: e.Mime, Mode: e.Mode}
	switch e.Kind {
	case tree.KindDir:
		n.Kind, n.Dir = gateway.KindDir, e.CID
	case tree.KindSymlink:
		n.Kind, n.Blob = gateway.KindSymlink, e.CID
	default:
		n.Kind, n.Blob = gateway.KindFile, e.CID
	}
	return n
}

// File is an open blob: a ReadSeeker over its bytes.
type File struct {
	*io.SectionReader
	b *store.Blob
}

// Close releases the blob.
func (f *File) Close() error { return f.b.Close() }

// Open opens the bytes of a file, blob or symlink node.
func (c *Content) Open(n *Node) (*File, error) {
	if !n.Blob.Defined() {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a directory", n.CID).WithAction("list it with `ls`")
	}
	b, err := c.st.OpenBlob(n.Blob.Multihash())
	if errors.Is(err, store.ErrNotFound) {
		return nil, NotHeld(n.Blob)
	}
	if err != nil {
		return nil, err
	}
	return &File{SectionReader: io.NewSectionReader(b, 0, int64(b.Size)), b: b}, nil
}

// Entries iterates a directory node in name order.
func (c *Content) Entries(n *Node) iter.Seq2[*Node, error] { return c.EntriesFrom(n, "") }

// EntriesFrom iterates a directory node in name order from the first name >= from.
func (c *Content) EntriesFrom(n *Node, from string) iter.Seq2[*Node, error] {
	return func(yield func(*Node, error) bool) {
		if n.Kind != gateway.KindDir {
			yield(nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not a directory", n.CID))
			return
		}
		err := tree.Dir(c.st, n.Dir).Walk(from, func(name string, e tree.Entry) (bool, error) {
			return yield(entryNode(name, e), nil), nil
		})
		if err != nil {
			yield(nil, notFoundPath(n.CID, "", err))
		}
	}
}

// gateway.Content.

func (c *Content) info(n *Node) *gateway.Info {
	return &gateway.Info{Kind: n.Kind, CID: n.CID.String(), Name: n.Name, Size: n.Size, Mime: n.Mime, Mode: n.Mode}
}

func parseRoot(s string) (cid.CID, error) {
	root, err := cid.Parse(s)
	if err != nil {
		return cid.CID{}, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", s)
	}
	return root, nil
}

// Gateway returns the gateway.Content view.
func (c *Content) Gateway() gateway.Content { return gw{c} }

type gw struct{ c *Content }

var _ gateway.Content = gw{}

func (g gw) Resolve(ctx context.Context, root, p string) (*gateway.Info, error) {
	rc, err := parseRoot(root)
	if err != nil {
		return nil, err
	}
	n, err := g.c.Resolve(rc, p)
	if err != nil {
		return nil, err
	}
	return g.c.info(n), nil
}

func (g gw) Open(ctx context.Context, info *gateway.Info) (gateway.File, error) {
	n, err := g.c.nodeOf(info)
	if err != nil {
		return nil, err
	}
	return g.c.Open(n)
}

func (g gw) List(ctx context.Context, info *gateway.Info) iter.Seq2[gateway.Entry, error] {
	return g.ListFrom(ctx, info, "")
}

var _ gateway.PagedContent = gw{}

// ListFrom implements gateway.PagedContent: the MST walk starts at from.
func (g gw) ListFrom(ctx context.Context, info *gateway.Info, from string) iter.Seq2[gateway.Entry, error] {
	return func(yield func(gateway.Entry, error) bool) {
		n, err := g.c.nodeOf(info)
		if err != nil {
			yield(gateway.Entry{}, err)
			return
		}
		for e, err := range g.c.EntriesFrom(n, from) {
			if err != nil {
				yield(gateway.Entry{}, err)
				return
			}
			if !yield(gateway.Entry{Name: e.Name, Kind: e.Kind, CID: e.CID.String(), Size: e.Size, Mime: e.Mime}, nil) {
				return
			}
		}
	}
}

// nodeOf re-resolves an Info (the gateway keeps only strings) as a root with no path.
func (c *Content) nodeOf(info *gateway.Info) (*Node, error) {
	root, err := parseRoot(info.CID)
	if err != nil {
		return nil, err
	}
	n, err := c.Resolve(root, "")
	if err != nil {
		return nil, err
	}
	n.Name, n.Mime, n.Mode = info.Name, info.Mime, info.Mode
	return n, nil
}

// Object opens the bytes of one held, complete object as stored: a DRISL node from the
// block table or a raw blob. It never fetches.
func (c *Content) Object(root cid.CID) (gateway.File, error) {
	if root.Codec() != cid.Raw {
		data, err := c.st.Get(root)
		if errors.Is(err, store.ErrNotFound) {
			return nil, NotHeld(root)
		}
		if err != nil {
			return nil, err
		}
		return bytesFile{bytes.NewReader(data)}, nil
	}
	b, err := c.st.OpenBlob(root.Multihash())
	if errors.Is(err, store.ErrNotFound) {
		return nil, NotHeld(root)
	}
	if err != nil {
		return nil, err
	}
	return &File{SectionReader: io.NewSectionReader(b, 0, int64(b.Size)), b: b}, nil
}

type bytesFile struct{ *bytes.Reader }

func (bytesFile) Close() error { return nil }

var _ gateway.ObjectContent = gw{}

// OpenObject implements gateway.ObjectContent (RASL).
func (g gw) OpenObject(ctx context.Context, s string) (gateway.File, error) {
	root, err := parseRoot(s)
	if err != nil {
		return nil, err
	}
	return g.c.Object(root)
}
