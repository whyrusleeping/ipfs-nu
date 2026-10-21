package gateway

import (
	"context"
	"io"
	"iter"
)

// Kind is what a resolved target is.
type Kind string

const (
	// KindFile is a file node: bytes with a name, MIME type and mode.
	KindFile Kind = "file"
	// KindDir is a directory.
	KindDir Kind = "dir"
	// KindBlob is raw bytes with no metadata.
	KindBlob Kind = "blob"
	// KindSymlink is a symlink entry; the gateway never serves or follows it.
	KindSymlink Kind = "symlink"
)

// Info describes a resolved target. Name, Mime and Mode come from the tree or file node
// and are untrusted: the gateway sanitizes them before use.
type Info struct {
	Kind Kind
	// CID is the CID of the resolved node (file node, directory or blob).
	CID  string
	Name string
	Size int64
	Mime string
	Mode uint32
}

// Entry is one row of a directory listing.
type Entry struct {
	Name string
	Kind Kind
	CID  string
	Size int64
	Mime string
}

// File is the bytes of a file or blob. Seek enables Range requests.
type File interface {
	io.ReadSeeker
	io.Closer
}

// Content is what the gateway needs from the node. Resolve walks root and the optional
// slash-separated path (already validated: no empty, "." or ".." segments) and returns
// apiv0.CodeNotFound when the content is not held or the path does not exist. The store
// implements it on trees, file nodes and blobs; tests use a fake.
type Content interface {
	Resolve(ctx context.Context, root, path string) (*Info, error)
	// Open returns the bytes of a file or blob Info.
	Open(ctx context.Context, info *Info) (File, error)
	// List iterates a directory Info in name order.
	List(ctx context.Context, info *Info) iter.Seq2[Entry, error]
}

// ObjectContent is implemented by content that can serve one held object's bytes as
// stored (a raw blob or a DRISL node) for RASL. It answers apiv0.CodeNotFound for
// anything not held and complete; it never fetches.
type ObjectContent interface {
	OpenObject(ctx context.Context, cid string) (File, error)
}
