package apiv0

import (
	"encoding/json"
	"time"
)

// Collection API types (docs/collections.md D22; docs/api.md "Collections"). This is the
// preview contract the item push API and dashboards build on.

// CollectionItem is one catalog item as this node sees it.
type CollectionItem struct {
	Path string `json:"path"`
	// Entry is the item's tree entry target, Kind and Size its kind and logical size,
	// Meta its metadata record.
	Entry  string `json:"entry,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Size   uint64 `json:"size,omitempty"`
	Meta   string `json:"meta,omitempty"`
	Writer string `json:"writer,omitempty"` // the writer whose head supplies the merged entry
	// Facets are the extracted facet values (times as RFC 3339 UTC).
	Facets map[string]any `json:"facets,omitempty"`
	// Removed marks an item that left the catalog (change stream only).
	Removed bool  `json:"removed,omitempty"`
	Seq     int64 `json:"seq"` // position in the item change stream
	// Rule is the index of the first local selection rule matching the item (-1: none).
	Rule int `json:"rule"`
	// Local is this node's holding state: fetching, complete, paused, or "" (not held).
	Local string `json:"local,omitempty"`
	// Copies counts holders with a fresh heartbeat that report the item complete; Unknown
	// counts complete holders with stale heartbeats. Both need the holdings index (nil
	// without one).
	Copies  *int `json:"copies,omitempty"`
	Unknown *int `json:"unknown,omitempty"`
	// Holders and Conflicts are filled by the single-item query.
	Holders   []CollectionHolder   `json:"holders,omitempty"`
	Conflicts []CollectionConflict `json:"conflicts,omitempty"`
}

// CollectionHolder is one member's reported holding of an item.
type CollectionHolder struct {
	Peer  string `json:"peer"`
	State string `json:"state"` // fetching, partial, complete
	Src   string `json:"src,omitempty"`
	Fresh bool   `json:"fresh"`
	Self  bool   `json:"self,omitempty"`
}

// CollectionConflict is a writer's entry that lost the union merge at a path.
type CollectionConflict struct {
	Path   string `json:"path"`
	Winner string `json:"winner,omitempty"`
	Loser  string `json:"loser"`
	Entry  string `json:"entry"`
	Reason string `json:"reason"` // entry or structure
}

// CollectionItemsQuery selects items. With Since (a change-stream cursor) the result is the
// stream of item changes in order; otherwise the current items matching the filters.
type CollectionItemsQuery struct {
	Since  string `json:"since,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Rarest bool   `json:"rarest,omitempty"`
	Holder string `json:"holder,omitempty"`
	Unheld bool   `json:"unheld,omitempty"`
	Held   bool   `json:"held,omitempty"` // held by this node
	Prefix string `json:"prefix,omitempty"`
	// Match is a rule in its flat JSON form whose predicates items must satisfy.
	Match json.RawMessage `json:"match,omitempty"`
}

// CollectionRules is a node's local selection rules for a collection (flat rule objects,
// docs/collections.md "Selection").
type CollectionRules struct {
	Rules  json.RawMessage `json:"rules"`
	DryRun []RuleImpact    `json:"dry_run,omitempty"`
	// Applied is false for a dry run.
	Applied bool `json:"applied"`
}

// RuleImpact is what one rule would hold: matching items and their logical bytes, and
// what its budget leaves out.
type RuleImpact struct {
	Rule         int    `json:"rule"`
	Items        int    `json:"items"`
	Bytes        uint64 `json:"bytes"`
	PausedItems  int    `json:"paused_items,omitempty"`
	PausedBytes  uint64 `json:"paused_bytes,omitempty"`
	Rescue       bool   `json:"rescue,omitempty"`
	Hold         bool   `json:"hold"`
	BudgetBytes  int64  `json:"budget_bytes,omitempty"`
	CurrentItems int    `json:"current_items"`
}

// CollectionStatus summarises a collection on this node.
type CollectionStatus struct {
	Name     string `json:"name"`
	Merge    string `json:"merge"`
	Replicas int64  `json:"replicas"`
	// Root is the merged catalog header; Writers the writer heads it merges, in rank order.
	Root    string             `json:"root,omitempty"`
	Writers []CollectionWriter `json:"writers"`
	Items   int                `json:"items"`
	Bytes   uint64             `json:"bytes"`
	// FacetAbsent counts items per facet whose value is missing or ill-typed.
	FacetAbsent map[string]int `json:"facet_absent,omitempty"`
	Conflicts   int            `json:"conflicts"`
	Rules       int            `json:"rules"`
	// Local holdings by state, with logical bytes.
	Local      map[string]int `json:"local"`
	LocalBytes uint64         `json:"local_bytes"`
	// Shortfall is the logical bytes of matched items paused by rule budgets.
	Shortfall uint64 `json:"shortfall_bytes,omitempty"`
	// Index reports the holdings index when this node runs one.
	Index *CollectionIndex `json:"index,omitempty"`
	// Log is this node's holdings-log position.
	LogEpoch string    `json:"log_epoch"`
	LogSeq   int64     `json:"log_seq"`
	Synced   time.Time `json:"synced,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
}

// CollectionWriter is one writer head merged into the catalog.
type CollectionWriter struct {
	DID    string `json:"did"`
	Head   string `json:"head"`
	Synced bool   `json:"synced"` // its catalog is held and merged
}

// CollectionIndex summarises the holdings index.
type CollectionIndex struct {
	Unheld  int                `json:"unheld"`
	Members []CollectionMember `json:"members"`
	Copies  map[string]int     `json:"copies_histogram"` // "0", "1", "2", … → items
}

// CollectionMember is one member's holdings as the index knows them.
type CollectionMember struct {
	Peer   string `json:"peer"`
	Fresh  bool   `json:"fresh"`
	Items  int    `json:"items"`
	Bytes  uint64 `json:"bytes"`
	Epoch  string `json:"epoch,omitempty"`
	Seq    int64  `json:"seq"`
	Behind bool   `json:"behind,omitempty"` // the member reports a later log position
}

// CollectionHoldRequest holds or releases an item (pin source collection-manual).
type CollectionHoldRequest struct {
	Path string `json:"path"`
}
