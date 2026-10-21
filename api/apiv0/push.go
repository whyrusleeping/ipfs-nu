package apiv0

import (
	"encoding/json"
	"time"
)

// Item push API types (docs/collections.md "Ingest"; docs/api.md "Item pushes").

// Push states. accepted: durably queued; fetching: files are being stored (or the
// built entry waits for the next head write); written: the entry is in this node's
// writer head; failed: no entry was written (see Error); removed: a DELETE took the
// item out of this node's writer head.
const (
	PushAccepted = "accepted"
	PushFetching = "fetching"
	PushWritten  = "written"
	PushFailed   = "failed"
	PushRemoved  = "removed"
)

// CollectionPushRequest adds one item to a collection: its path, its metadata record
// (an atproto-style JSON object, or the CID string of a held DRISL record) and its files
// keyed by their path inside the item. Replace supersedes a different entry at the path
// in this node's writer head; without it a different entry is a conflict.
type CollectionPushRequest struct {
	Path    string                        `json:"path"`
	Meta    json.RawMessage               `json:"meta,omitempty"`
	Files   map[string]CollectionPushFile `json:"files"`
	Replace bool                          `json:"replace,omitempty"`
}

// CollectionPushFile is one file of a pushed item: exactly one of Upload (a blob CID
// returned by POST …/uploads), Path (an absolute path on the receiving node, referenced
// in place with NoCopy) or URL (fetched by URL import, verified against Checksum, a hex
// multihash such as STAC file:checksum, and Size when given).
type CollectionPushFile struct {
	Upload   string `json:"upload,omitempty"`
	Path     string `json:"path,omitempty"`
	NoCopy   bool   `json:"nocopy,omitempty"`
	URL      string `json:"url,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// CollectionPush is a push's state on the receiving node.
type CollectionPush struct {
	Path string `json:"path"`
	// ID names the accepted push; an identical resend answers the same ID with
	// Duplicate set.
	ID        string `json:"id"`
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Replace   bool   `json:"replace,omitempty"`
	// Entry is the item's directory once its files are stored, Size its logical size,
	// Meta its metadata record.
	Entry string `json:"entry,omitempty"`
	Size  uint64 `json:"size,omitempty"`
	Meta  string `json:"meta,omitempty"`
	// Head is the writer head (tree header) the entry was written into.
	Head string `json:"head,omitempty"`
	// Sources is the provenance of URL files, keyed like the request's files.
	Sources map[string]URLSource `json:"sources,omitempty"`
	// Tries counts failed fetch attempts so far; Error is set when failed (code conflict
	// when a different entry is at the path) or while retrying.
	Tries   int       `json:"tries,omitempty"`
	Error   *Error    `json:"error,omitempty"`
	Seq     int64     `json:"seq"` // position in the push log
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// URLSource is the provenance of a URL import: the URL asked for, the final URL after
// redirects, the response validators and type, and the bytes received. Validators are
// HTTP metadata, not integrity proofs.
type URLSource struct {
	URL          string `json:"url"`
	FinalURL     string `json:"final_url,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum,omitempty"` // the verified checksum, when one was supplied
	Resumed      bool   `json:"resumed,omitempty"`
}

// CollectionPushBatchLine is one line of an items:batch response: the push for request
// line Line (1-based), or the error that rejected it.
type CollectionPushBatchLine struct {
	Line  int             `json:"line"`
	Path  string          `json:"path,omitempty"`
	Push  *CollectionPush `json:"push,omitempty"`
	Error *Error          `json:"error,omitempty"`
}

// CollectionUpload is a blob stored by POST …/uploads, to be referenced by a push
// within the store's GC grace (an hour).
type CollectionUpload struct {
	CID  string `json:"cid"`
	Size int64  `json:"size"`
}

// PushWait bounds ?wait=written.
const (
	DefaultPushWait = 30 * time.Second
	MaxPushWait     = 5 * time.Minute
)
