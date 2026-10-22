// Package apiv0 defines the wire types of local API version 0: the error envelope with its
// stable codes and CLI exit categories, cursor pages, operations and their events, and the
// request/response bodies of the content endpoints. The server (api), the Go client (client)
// and the node seam (node) share these types; the package has no I/O.
package apiv0

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Prefix is the URL prefix of every v0 route.
const Prefix = "/api/v0"

// OpenAPIPath is the stable path of the OpenAPI document describing this version.
const OpenAPIPath = Prefix + "/openapi.json"

// Code is a stable error code. Codes are the contract for scripts and the CLI's exit
// categories; messages and actions are free text and may change.
type Code string

const (
	// CodeFailed is an unclassified failure (internal error, failed operation).
	CodeFailed Code = "failed"
	// CodeInvalidArgument covers malformed input: bad target syntax, bad flags, bad JSON.
	CodeInvalidArgument Code = "invalid_argument"
	// CodeNotFound: the target, operation or route does not exist.
	CodeNotFound Code = "not_found"
	// CodeNotSupported: valid input that this version cannot act on yet (for example a
	// name target before names ship).
	CodeNotSupported Code = "not_supported"
	// CodeNotImplemented: the endpoint exists but is not implemented in this build.
	CodeNotImplemented Code = "not_implemented"
	// CodeConflict: the request conflicts with current state (daemon already running,
	// operation already finished).
	CodeConflict Code = "conflict"
	// CodeTimeout: a wait expired before the operation finished.
	CodeTimeout Code = "timeout"
	// CodeUnavailable: no daemon is reachable, or it is shutting down.
	CodeUnavailable Code = "unavailable"
	// CodeForbidden: the request failed the loopback/origin protections or authentication.
	CodeForbidden Code = "forbidden"
	// CodeCancelled: the operation was cancelled or the command interrupted.
	CodeCancelled Code = "cancelled"
	// CodeInsufficientStorage: a store budget refused the write (D17 A6). Details carry
	// the shortfall: reason (store_limit|free_space), need, headroom, physical, limit,
	// free, reserve.
	CodeInsufficientStorage Code = "insufficient_storage"
)

// Exit categories. Every CLI command exits with one of these; a code's category is
// ExitCode(code). Flag parsing errors exit with ExitUsage like Go's flag package.
const (
	ExitOK                  = 0
	ExitFailed              = 1
	ExitUsage               = 2
	ExitNotFound            = 3
	ExitNotSupported        = 4
	ExitConflict            = 5
	ExitTimeout             = 6
	ExitUnavailable         = 7
	ExitForbidden           = 8
	ExitCancelled           = 9
	ExitNotImplemented      = 10
	ExitInsufficientStorage = 11
)

// ExitCode maps an error code to the CLI exit category. Unknown codes map to ExitFailed.
func ExitCode(c Code) int {
	switch c {
	case CodeInvalidArgument:
		return ExitUsage
	case CodeNotFound:
		return ExitNotFound
	case CodeNotSupported:
		return ExitNotSupported
	case CodeNotImplemented:
		return ExitNotImplemented
	case CodeConflict:
		return ExitConflict
	case CodeTimeout:
		return ExitTimeout
	case CodeUnavailable:
		return ExitUnavailable
	case CodeForbidden:
		return ExitForbidden
	case CodeCancelled:
		return ExitCancelled
	case CodeInsufficientStorage:
		return ExitInsufficientStorage
	}
	return ExitFailed
}

// HTTPStatus maps an error code to the HTTP status the server answers with.
func HTTPStatus(c Code) int {
	switch c {
	case CodeInvalidArgument:
		return 400
	case CodeNotFound:
		return 404
	case CodeNotSupported:
		return 422
	case CodeNotImplemented:
		return 501
	case CodeConflict:
		return 409
	case CodeTimeout:
		return 408
	case CodeUnavailable:
		return 503
	case CodeForbidden:
		return 403
	case CodeCancelled:
		return 409
	case CodeInsufficientStorage:
		return 507
	}
	return 500
}

// Error is the single error shape of the API and the CLI. Message says what happened,
// Action what to do next, both in plain language. Details carry machine-readable context.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Action  string         `json:"action,omitempty"`
	Details map[string]any `json:"details,omitempty"`

	cause error // in-process only: errors.Is/As see through the envelope
}

// WithCause returns a copy of e that unwraps to cause (never serialized).
func (e *Error) WithCause(cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

func (e *Error) Unwrap() error { return e.cause }

// Envelope is the JSON body of every error response: {"error": {...}}.
type Envelope struct {
	Error *Error `json:"error"`
}

func (e *Error) Error() string {
	if e.Action != "" {
		return fmt.Sprintf("%s (%s): %s", e.Message, e.Code, e.Action)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// Errorf builds an Error with a formatted message and no action.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithAction returns a copy of e with the action set.
func (e *Error) WithAction(action string) *Error {
	c := *e
	c.Action = action
	return &c
}

// WithDetail returns a copy of e with one detail added.
func (e *Error) WithDetail(key string, value any) *Error {
	c := *e
	c.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		c.Details[k] = v
	}
	c.Details[key] = value
	return &c
}

// NotImplemented is the error for an endpoint or command that this build does not have
// yet (for example `get` before extraction is wired to fetching).
func NotImplemented(what string) *Error {
	return &Error{
		Code:    CodeNotImplemented,
		Message: what + " is not implemented in this build",
		Action:  "this is a pre-v0 build; follow the project for a release that includes it",
	}
}

// Page is the body of every list endpoint. NextCursor is opaque and empty on the last
// page. Resync is true when the request's cursor expired: the client must drop what it
// has and list again from the beginning instead of treating the page as an empty delta.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Resync     bool   `json:"resync,omitempty"`
}

// State is the lifecycle state of an operation.
type State string

const (
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool { return s != StateRunning && s != "" }

// Progress is a bounded counter. Total is 0 when unknown. Unit is "bytes", "items" or
// another plain word used in progress rendering.
type Progress struct {
	Done  int64  `json:"done"`
	Total int64  `json:"total,omitempty"`
	Unit  string `json:"unit,omitempty"`
}

// Operation is long-running work with an ID, an exact target, a stage, progress, an
// event stream and a wait. Fetches, adds, follow syncs, item pushes, migrations and
// bundles are all operations.
type Operation struct {
	ID string `json:"id"`
	// Kind names the work: "add", "fetch", "pin", "follow", ...
	Kind string `json:"kind"`
	// Target is the exact thing being worked on (a CID, path or link), never a name that
	// could resolve differently later.
	Target   string    `json:"target"`
	State    State     `json:"state"`
	Stage    string    `json:"stage,omitempty"`
	Progress Progress  `json:"progress"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	// Result is kind-specific and present only when State is succeeded.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is present only when State is failed or cancelled.
	Error *Error `json:"error,omitempty"`
}

// Done reports whether the operation reached a terminal state.
func (o Operation) Done() bool { return o.State.Terminal() }

// EventType classifies operation events.
type EventType string

const (
	// EventStage: the stage changed; Stage is set.
	EventStage EventType = "stage"
	// EventProgress: the counters changed; Progress is set.
	EventProgress EventType = "progress"
	// EventLog: a human-readable line; Message is set.
	EventLog EventType = "log"
	// EventDone: the operation reached a terminal state; Operation holds the final
	// snapshot. It is always the last event of a stream.
	EventDone EventType = "done"
)

// Event is one line of an operation's NDJSON stream. Seq increases by one per event and
// is the `since` cursor for resuming a stream.
type Event struct {
	Seq       int64      `json:"seq"`
	Time      time.Time  `json:"time"`
	Type      EventType  `json:"type"`
	Stage     string     `json:"stage,omitempty"`
	Progress  *Progress  `json:"progress,omitempty"`
	Message   string     `json:"message,omitempty"`
	Operation *Operation `json:"operation,omitempty"`
}

// EventStream delivers operation events in order. Next returns io.EOF after the done
// event. Close releases the stream; it is safe to call after io.EOF.
type EventStream interface {
	Next(ctx context.Context) (*Event, error)
	Close() error
}

// Status is the daemon summary. Later tasks add sections (reachability, shares, datasets,
// cluster); existing fields keep their meaning.
type Status struct {
	Version string `json:"version"`
	Daemon  Daemon `json:"daemon"`
	Paths   Paths  `json:"paths"`
	// Operations counts operations by state.
	Operations map[State]int `json:"operations"`
	// Store summarises the local store.
	Store *StoreStatus `json:"store,omitempty"`
	// Network is the node's identity and reachability.
	Network *NetworkStatus `json:"network,omitempty"`
}

// NetworkStatus is the network section of Status: who this node is and how it can be
// reached. Serving and Peers stay false/0 until a libp2p host runs in the process.
type NetworkStatus struct {
	// PeerID is the node's base36 libp2p PeerId (the `peer=` hint of its share links).
	PeerID string `json:"peer_id"`
	// Addrs are the multiaddrs the node would advertise: the host's actual addresses when
	// one runs, else the swarm listen config expanded over the machine's non-loopback
	// interfaces.
	Addrs []string `json:"addrs"`
	// Serving reports whether a host is accepting connections.
	Serving bool `json:"serving"`
	// Peers counts connected peers.
	Peers int `json:"peers"`
	// Connected lists the connected peers' base36 PeerIds.
	Connected []string `json:"connected,omitempty"`
}

// StoreStatus is the store section of Status (D17 A8). Byte fields are physical: what
// the store occupies on disk, each object charged once.
type StoreStatus struct {
	// Objects counts held objects (blocks, blobs and partial blobs).
	Objects int64 `json:"objects"`
	// Bytes is the physical total: blobs + outboards + partials + db + staging.
	Bytes int64 `json:"bytes"`
	Pins  int64 `json:"pins"`
	// Blobs is complete copied blob data, Outboards the Bao outboards, Partials the
	// verified bytes of partial blobs, DB the database with its WAL, Staging in-progress
	// writes, Reserved admitted but unwritten fetch bytes.
	Blobs     int64 `json:"blobs"`
	Outboards int64 `json:"outboards"`
	Partials  int64 `json:"partials"`
	DB        int64 `json:"db"`
	Staging   int64 `json:"staging"`
	Reserved  int64 `json:"reserved"`
	// NoCopy is the bytes of --nocopy sources referenced in place, not charged.
	NoCopy int64 `json:"nocopy"`
	// Free is the free space on the store's filesystem (-1 unknown).
	Free int64 `json:"free"`
	// Limit is store.limit (0 unlimited); Reserve is store.free_reserve.
	Limit   int64 `json:"limit"`
	Reserve int64 `json:"reserve"`
	// Headroom is what new writes may still use (-1 unlimited); Pressure is set while it
	// is 0, when acquisition pauses with insufficient_storage.
	Headroom int64 `json:"headroom"`
	Pressure bool  `json:"pressure"`
}

// Daemon describes the running process.
type Daemon struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	// API and Gateway are the bound listen addresses as host:port.
	API     string `json:"api"`
	Gateway string `json:"gateway"`
	// Public is the public listener's origin (https://<domain>) when it is running.
	Public string `json:"public,omitempty"`
}

// Paths lists the `$NU_PATH` layout.
type Paths struct {
	Root    string `json:"root"`
	Config  string `json:"config"`
	Key     string `json:"key"`
	DB      string `json:"db"`
	Blobs   string `json:"blobs"`
	APIFile string `json:"api_file"`
}

// IPFSImportRequest starts a verified IPFS CAR import.
type IPFSImportRequest struct {
	Root   string `json:"root"`
	CAR    string `json:"car"`
	DryRun bool   `json:"dry_run,omitempty"`
}

// AddRequest starts an add operation. The daemon reads Path itself (API and daemon run as
// the same user on loopback), so no upload is needed. The result of the operation is an
// AddResult. Fields mirror `nu add` flags and will be settled by the store integration.
type AddRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
	// Raw adds a whole-file blob instead of a file node with metadata.
	Raw bool `json:"raw,omitempty"`
	// Hash is "" (BLAKE3 default) or "sha256" (core-DASL blob, ≤ 8 MiB).
	Hash string `json:"hash,omitempty"`
	// NoCopy references the local file instead of copying it into the store.
	NoCopy bool `json:"nocopy,omitempty"`
	// NoAnnounce adds without providing to the network.
	NoAnnounce bool `json:"no_announce,omitempty"`
	// Pin pins the added root (default true in the CLI).
	Pin bool `json:"pin,omitempty"`
	// URL imports an https:// URL instead of reading Path (D17 URL import): the bytes
	// are hashed on arrival and the result records their provenance.
	URL string `json:"url,omitempty"`
	// Meta is a metadata record (an atproto-style JSON object) for the added content;
	// the result's Entry carries it. A URL import adds its provenance under "source"
	// unless the record already has that key.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// TreeEntry is a tree entry for added content with a metadata record (what `nu tree
// edit --put` takes).
type TreeEntry struct {
	Kind string `json:"kind"`
	CID  string `json:"cid"`
	Size uint64 `json:"size"`
	Meta string `json:"meta,omitempty"`
}

// AddResult is the result of an add operation.
type AddResult struct {
	CID  string `json:"cid"`
	Name string `json:"name,omitempty"`
	// Size is the content size in bytes: the file's length, or the tree's total.
	Size int64 `json:"size"`
	// Files counts file entries in an added tree.
	Files int64 `json:"files,omitempty"`
	// Skipped lists unsupported entries (devices, sockets, FIFOs) left out of a tree.
	Skipped []string `json:"skipped,omitempty"`
	// Link is the share link, when the daemon can produce one.
	Link string `json:"link,omitempty"`
	// Source is the provenance of a URL import.
	Source *URLSource `json:"source,omitempty"`
	// Meta is the stored metadata record and Entry the tree entry carrying it.
	Meta  string     `json:"meta,omitempty"`
	Entry *TreeEntry `json:"entry,omitempty"`
}

// Entry is one row of a directory listing.
type Entry struct {
	Name string `json:"name"`
	CID  string `json:"cid"`
	// Type is "file", "dir", "symlink" or "blob".
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

// Listing is the body of an ls.
type Listing struct {
	// Target is the resolved CID (with path) that was listed.
	Target  string  `json:"target"`
	Entries []Entry `json:"entries"`
}

// Pin is one row of the pin list.
type Pin struct {
	CID  string `json:"cid"`
	Name string `json:"name,omitempty"`
	// Mode is "recursive" or "direct"; Source is who holds the pin ("explicit" for
	// `pin add` and `add`; follow, cluster and collection sources arrive later).
	Mode   string `json:"mode,omitempty"`
	Source string `json:"source,omitempty"`
}

// PinRequest adds a pin; the daemon may need to fetch the content, so it is an operation.
type PinRequest struct {
	CID  string `json:"cid"`
	Name string `json:"name,omitempty"`
}

// DagPutResult is the result of storing one DRISL object.
type DagPutResult struct {
	CID string `json:"cid"`
}

// GetRequest starts a get operation: the target (a CID with optional path, or a nu://
// share link, kept exact as the operation's target) is written to disk with safe
// extraction. Like add, the daemon writes the path itself. Output is the exact destination
// path; when empty, the content is written into Dir under its own name (the file node's
// name, the path's last segment, or the CID). One of Output and Dir is required, as an
// absolute path.
type GetRequest struct {
	Target string `json:"target"`
	Output string `json:"output,omitempty"`
	Dir    string `json:"dir,omitempty"`
}

// GetResult is the result of a get operation.
type GetResult struct {
	// CID is the resolved root that was written (with Path, when the target had one).
	CID  string `json:"cid"`
	Path string `json:"path,omitempty"`
	// Output is the absolute path written.
	Output string `json:"output"`
	// Kind is "file", "dir" or "blob".
	Kind  string `json:"kind"`
	Files int    `json:"files"`
	Dirs  int    `json:"dirs"`
	Bytes int64  `json:"bytes"`
	// Skipped lists entries not written (symlinks) and why.
	Skipped []string `json:"skipped,omitempty"`
	// Fetched reports whether any of the content had to be fetched from other nodes.
	Fetched bool `json:"fetched,omitempty"`
}

// ShareRequest asks for the share link of held content.
type ShareRequest struct {
	// Target is a CID with optional path.
	Target string `json:"target"`
}

// ShareLink is the answer to a share: the link to paste, and what went into it.
type ShareLink struct {
	Target string `json:"target"`
	Link   string `json:"link"`
	PeerID string `json:"peer_id"`
	// Addrs are the multiaddrs included as hints.
	Addrs []string `json:"addrs"`
	// Serving reports whether this node is accepting connections for the content now.
	Serving bool `json:"serving"`
	// HTTPS, Site and RASL are set only when this node's public listener serves the
	// root now (operations.md P2): the bare-origin browser URL, the site origin for a
	// tree, and the rasl:// link with the seed as hint.
	HTTPS string `json:"https,omitempty"`
	Site  string `json:"site,omitempty"`
	RASL  string `json:"rasl,omitempty"`
}
