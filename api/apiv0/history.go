package apiv0

import "time"

// Version history types (docs/cluster.md "Version history"; 0047).

// HistoryPolicy is a dataset's retention schedule: the KeepLast newest versions, then the
// newest version in each of the first N hour, day, week, month and year buckets (-1:
// all), plus labeled versions and the head's; MaxBytes (0: none) bounds the live set's
// summed new bytes.
type HistoryPolicy struct {
	KeepLast int64 `json:"keep_last"`
	Hourly   int64 `json:"hourly"`
	Daily    int64 `json:"daily"`
	Weekly   int64 `json:"weekly"`
	Monthly  int64 `json:"monthly"`
	Yearly   int64 `json:"yearly"`
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// Version availability (docs/cluster.md "Availability"), reported apart from readiness.
const (
	AvailComplete = "retained-complete"
	AvailFetching = "fetching"
	AvailObserved = "observed"
	AvailDegraded = "degraded"
	AvailUnknown  = "unknown"
	AvailSkipped  = "skipped"
	AvailPruned   = "pruned"
)

// VersionInfo is one version: its immutable identity (TID, root, kind, parent, author,
// sizes, restored_from), its annotations (label, note, by annotated_by), whether it is in
// the live set and why, and its availability as this node sees it. Conflicts lists
// records that tried to redefine the version (never merged into it).
type VersionInfo struct {
	TID          string            `json:"tid"`
	Time         time.Time         `json:"time"`
	Root         string            `json:"root"`
	Kind         string            `json:"kind"`
	Parent       string            `json:"parent,omitempty"`
	Author       string            `json:"author"`
	Files        int64             `json:"files"`
	Bytes        int64             `json:"bytes"`
	NewBytes     int64             `json:"new_bytes"`
	RestoredFrom string            `json:"restored_from,omitempty"`
	Label        string            `json:"label,omitempty"`
	Note         string            `json:"note,omitempty"`
	AnnotatedBy  string            `json:"annotated_by,omitempty"`
	Head         bool              `json:"head,omitempty"`
	Live         bool              `json:"live"`
	Reasons      []string          `json:"reasons,omitempty"`
	Availability string            `json:"availability"`
	Detail       string            `json:"detail,omitempty"`
	Conflicts    []VersionConflict `json:"conflicts,omitempty"`

	// Imported is the signed reference to an import's provenance manifest (0042).
	Imported *VersionImported `json:"imported,omitempty"`
}

// VersionImported is a version's import provenance: the original root and manifest CID.
type VersionImported struct {
	Source   string `json:"source"`
	Manifest string `json:"manifest"`
}

// VersionConflict is a record for the same TID whose identity differs.
type VersionConflict struct {
	Signer string `json:"signer"`
	Root   string `json:"root"`
	Author string `json:"author"`
}

// DatasetVersions is a dataset's history: the schedule, the live-set identity with its
// summed new bytes (and the protected excess over max_bytes), archive-wide readiness
// over the live set (E8; separate from each version's availability) and the versions in
// TID order.
type DatasetVersions struct {
	Name      string         `json:"name"`
	History   *HistoryPolicy `json:"history,omitempty"`
	Live      string         `json:"live,omitempty"`
	LiveBytes int64          `json:"live_bytes"`
	Excess    int64          `json:"excess_bytes,omitempty"`
	Archive   *ContentState  `json:"archive,omitempty"`
	Versions  []VersionInfo  `json:"versions"`
}

// VersionLabelRequest labels a version (or removes its label with Remove). Labeling a
// version whose content is not complete needs Reacquire (it then becomes live again).
type VersionLabelRequest struct {
	Version   string `json:"version"`
	Label     string `json:"label,omitempty"`
	Note      string `json:"note,omitempty"`
	Remove    bool   `json:"remove,omitempty"`
	Reacquire bool   `json:"reacquire,omitempty"`
}

// VersionLabel is a label write's result: the version and the state write's metadata.
type VersionLabel struct {
	Version  VersionInfo   `json:"version"`
	Metadata MetadataState `json:"metadata"`
}

// RollbackRequest writes a new head restoring a version.
type RollbackRequest struct {
	Version string `json:"version"`
	Note    string `json:"note,omitempty"`
}

// DatasetDiff lists the paths added, removed and changed from one version to another
// (at most 10000 in total; Truncated says more exist).
type DatasetDiff struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Changed   []string `json:"changed"`
	Truncated bool     `json:"truncated,omitempty"`
}

// PruneRequest prunes versions outside the live set; DryRun only reports.
type PruneRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// PruneReport lists the prune candidates (versions outside the live set whose content is
// still pinned) with the bytes releasing them all would free (objects no other pin
// reaches) and the bytes they share with other pins (kept), and the versions released.
type PruneReport struct {
	DryRun     bool             `json:"dry_run"`
	Candidates []PruneCandidate `json:"candidates"`
	FreeBytes  int64            `json:"free_bytes"`
	SharedKept int64            `json:"shared_bytes"`
	Pruned     []string         `json:"pruned,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
}

// PruneCandidate is one version a prune would release.
type PruneCandidate struct {
	TID   string `json:"tid"`
	Root  string `json:"root"`
	Label string `json:"label,omitempty"`
}
