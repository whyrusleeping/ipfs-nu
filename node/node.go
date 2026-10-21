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
}

// EventStream delivers operation events in order; see apiv0.EventStream.
type EventStream = apiv0.EventStream
