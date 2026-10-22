package apiv0

// IPFSImportResult describes verified source-to-Nu conversion of one root, or its dry run.
type IPFSImportResult struct {
	IPFSRoot                 string `json:"ipfs_root"`
	Root                     string `json:"root,omitempty"`
	Manifest                 string `json:"manifest,omitempty"`
	Source                   string `json:"source,omitempty"` // car or bitswap
	Files                    uint64 `json:"files"`
	Bytes                    uint64 `json:"bytes"`
	DryRun                   bool   `json:"dry_run,omitempty"`
	Blocks                   int    `json:"blocks,omitempty"`
	EstimatedPhysicalBytes   uint64 `json:"estimated_blob_physical_bytes,omitempty"`
	StoreHeadroom            int64  `json:"store_headroom,omitempty"`
	BlobEstimateFitsHeadroom *bool  `json:"blob_estimate_fits_headroom,omitempty"`
	// Dry-run inventory: Complete is false when structural blocks were missing (counts
	// then cover only what was reached); network dry runs report declared sizes.
	Complete        *bool      `json:"complete,omitempty"`
	StructureBlocks int        `json:"structure_blocks,omitempty"`
	Directories     uint64     `json:"directories,omitempty"`
	Symlinks        uint64     `json:"symlinks,omitempty"`
	Missing         []IPFSItem `json:"missing,omitempty"`
	Unsupported     []IPFSItem `json:"unsupported,omitempty"`
	Transformations []IPFSItem `json:"transformations,omitempty"`
	// Dataset is the head/version write of an import with a dataset.
	Dataset *Dataset `json:"dataset,omitempty"`
}

// IPFSItem is one reported path of an import: a metadata transformation, a missing block
// or an unsupported object.
type IPFSItem struct {
	Path   string `json:"path"`
	CID    string `json:"cid,omitempty"`
	Reason string `json:"reason"`
}

// Per-root reconciliation statuses of a pinset import.
const (
	IPFSImported    = "imported"
	IPFSPlanned     = "planned" // dry run: supported and complete
	IPFSMissing     = "missing"
	IPFSUnsupported = "unsupported"
	IPFSFailed      = "failed"
	IPFSSkipped     = "skipped" // not a recursive pin
)

// IPFSPinsetResult is the reconciliation report of a pinset import: one entry per pin of
// the inventory, in inventory (CID) order, and the counts per status.
type IPFSPinsetResult struct {
	Inventory                string           `json:"inventory"`
	Pins                     int              `json:"pins"`
	DryRun                   bool             `json:"dry_run,omitempty"`
	Counts                   map[string]int   `json:"counts"`
	Bytes                    uint64           `json:"bytes"`
	EstimatedPhysicalBytes   uint64           `json:"estimated_blob_physical_bytes,omitempty"`
	StoreHeadroom            int64            `json:"store_headroom,omitempty"`
	BlobEstimateFitsHeadroom *bool            `json:"blob_estimate_fits_headroom,omitempty"`
	Roots                    []IPFSRootReport `json:"roots"`
}

// IPFSRootReport is one pin's outcome.
type IPFSRootReport struct {
	IPFSRoot string `json:"ipfs_root"`
	Name     string `json:"name,omitempty"`
	PinType  string `json:"pin_type,omitempty"`
	Status   string `json:"status"`
	Root     string `json:"root,omitempty"`
	Manifest string `json:"manifest,omitempty"`
	Files    uint64 `json:"files,omitempty"`
	Bytes    uint64 `json:"bytes,omitempty"`
	// EstimatedPhysicalBytes is set by dry runs.
	EstimatedPhysicalBytes uint64     `json:"estimated_blob_physical_bytes,omitempty"`
	Missing                []IPFSItem `json:"missing,omitempty"`
	Unsupported            []IPFSItem `json:"unsupported,omitempty"`
	Transformations        []IPFSItem `json:"transformations,omitempty"`
	Error                  string     `json:"error,omitempty"`
}

// ImportCutoverRequest checks whether an imported dataset head may replace its IPFS
// source (POST /import/ipfs/cutover).
type ImportCutoverRequest struct {
	Scope   string `json:"scope,omitempty"` // local (default) or cluster
	Dataset string `json:"dataset"`
	// Timeout bounds the readiness wait (default 10m; 0 waits indefinitely).
	Timeout string `json:"timeout,omitempty"`
}

// ImportCutoverResult reports the cutover gates in order: the head version's signed import
// reference, a fresh client's network retrieval, and exact-policy readiness. Recommend is
// true only when all pass; Missing says what is not yet true.
type ImportCutoverResult struct {
	Dataset   string            `json:"dataset"`
	Scope     string            `json:"scope"`
	Version   string            `json:"version,omitempty"`
	Target    string            `json:"target,omitempty"`
	Imported  *VersionImported  `json:"imported,omitempty"`
	Retrieval *CutoverRetrieval `json:"retrieval,omitempty"`
	Readiness *WaitResult       `json:"readiness,omitempty"`
	Recommend bool              `json:"recommend"`
	Missing   []string          `json:"missing,omitempty"`
	Notes     []string          `json:"notes,omitempty"`
}

// CutoverRetrieval is the fresh-client check: a new node with its own empty store and key
// fetched Target over the network (never from the daemon's store directly).
type CutoverRetrieval struct {
	OK        bool     `json:"ok"`
	Client    string   `json:"client,omitempty"` // the fresh client's PeerId
	Providers []string `json:"providers,omitempty"`
	Files     int64    `json:"files,omitempty"`
	Bytes     int64    `json:"bytes,omitempty"`
	Error     string   `json:"error,omitempty"`
}
