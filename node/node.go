// Package node assembles a Nu node from the store, networking, fetcher, API and gateway.
//
// Node is the seam between commands and a running node: the HTTP client (package client)
// implements it against a daemon, and Local implements it in-process. Everything the CLI
// does goes through this interface, so anything the CLI can do the API can do too.
package node

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
)

// Node is what commands need from a node. Methods that touch content (Add, Cat, Ls, pins,
// dag) read and write the local store; fetching from other nodes arrives with sharing.
type Node interface {
	// Status returns the daemon summary.
	Status(ctx context.Context) (*apiv0.Status, error)

	// Operations lists retained operations, oldest first, paged by opaque cursor.
	Operations(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Operation], error)
	// Operation returns one operation by ID.
	Operation(ctx context.Context, id string) (*apiv0.Operation, error)
	// OperationEvents streams an operation's events after seq `since` (0 for all) until the
	// done event; the stream then returns io.EOF.
	OperationEvents(ctx context.Context, id string, since int64) (EventStream, error)
	// WaitOperation blocks until the operation finishes or timeout elapses (0: no limit),
	// returning apiv0.CodeTimeout with the current snapshot in details on expiry.
	WaitOperation(ctx context.Context, id string, timeout time.Duration) (*apiv0.Operation, error)
	// CancelOperation cancels a running operation.
	CancelOperation(ctx context.Context, id string) error

	// Add starts an add operation whose result is an apiv0.AddResult.
	Add(ctx context.Context, req apiv0.AddRequest) (*apiv0.Operation, error)
	// ImportIPFS starts a verified CAR-to-Nu migration operation.
	ImportIPFS(ctx context.Context, req apiv0.IPFSImportRequest) (*apiv0.Operation, error)
	// ImportCutover checks an imported dataset's cutover gates (fresh-client retrieval,
	// exact-policy readiness) before recommending it replace its IPFS source.
	ImportCutover(ctx context.Context, req apiv0.ImportCutoverRequest) (*apiv0.ImportCutoverResult, error)
	// Cat streams the bytes of a CID target (file node, blob, or path inside a tree).
	Cat(ctx context.Context, target string) (io.ReadCloser, error)
	// Ls lists a directory target.
	Ls(ctx context.Context, target string) (*apiv0.Listing, error)
	// PinAdd starts a pin operation (it may fetch); the result is an apiv0.Pin.
	PinAdd(ctx context.Context, req apiv0.PinRequest) (*apiv0.Operation, error)
	// PinRm removes a pin.
	PinRm(ctx context.Context, cid string) error
	// Pins lists pins, paged by opaque cursor.
	Pins(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Pin], error)
	// DagGet returns a DRISL object (or a path into one) as atproto-style JSON.
	DagGet(ctx context.Context, target string) (json.RawMessage, error)
	// DagPut stores atproto-style JSON as one DRISL object and returns its CID.
	DagPut(ctx context.Context, doc io.Reader) (*apiv0.DagPutResult, error)

	// Get starts a get operation (result: apiv0.GetResult) that writes a CID or share-link
	// target to disk, fetching it first when it is not held.
	Get(ctx context.Context, req apiv0.GetRequest) (*apiv0.Operation, error)
	// Share returns the share link of held content, with this node as the provider hint.
	Share(ctx context.Context, req apiv0.ShareRequest) (*apiv0.ShareLink, error)
}

// EventStream delivers operation events in order; see apiv0.EventStream.
type EventStream = apiv0.EventStream

// Fetcher brings content the node does not hold into its store. The node asks it from
// get (and later cat, ls and pin) when the target or part of it is missing; the
// multi-peer fetcher (package fetch) implements it, and Local falls back to a Fetcher that
// answers not_implemented until one is plugged in.
type Fetcher interface {
	// Fetch makes root and everything reachable from it under the nu-tree walk profile
	// held and complete in the store. hints are the providers named by a share link (may
	// be empty: the fetcher then relies on lookup). progress, if non-nil, is called with
	// bytes fetched so far and the total when known (0 otherwise); callers rate-limit
	// nothing, so implementations should call it at most a few times per second. The
	// first call comes only once all structure under root is held (blobs may still be
	// arriving): `get` starts writing files then. Fetch
	// returns when the content is held, or an *apiv0.Error (not_found when no provider
	// has it, unavailable when none is reachable) wrapping the cause.
	Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error
}

// NoFetcher is the Fetcher of a build or node without networking: every Fetch fails with
// not_implemented and the action to add the content locally.
type NoFetcher struct{}

func (NoFetcher) Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error {
	return apiv0.NotImplemented("fetching " + root.String() + " from other nodes").
		WithAction("add the content on this machine, or wait for a build with networking")
}
