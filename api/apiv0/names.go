package apiv0

import (
	"encoding/json"
	"time"
)

// Public names, login and following (0045; docs/names.md D4–D7, docs/follow.md).

// LoginRequest starts an atproto OAuth login for a handle or DID. The daemon serves the
// loopback callback; the result names the URL to open in a browser.
type LoginRequest struct {
	Identifier string `json:"identifier"`
}

// LoginStart is a started login: open URL in a browser, then wait for Operation
// (result: Account).
type LoginStart struct {
	Operation Operation `json:"operation"`
	URL       string    `json:"url"`
	DID       string    `json:"did"`
	Handle    string    `json:"handle,omitempty"`
	PDS       string    `json:"pds"`
}

// Account is a logged-in atproto account (tokens are never exposed).
type Account struct {
	DID      string    `json:"did"`
	Handle   string    `json:"handle,omitempty"`
	PDS      string    `json:"pds"`
	Scopes   []string  `json:"scopes"`
	LoggedIn time.Time `json:"logged_in"`
	Updated  time.Time `json:"updated"`
	// Node is this node's record in the account (`nu node register`), if any.
	Node *NodeRecord `json:"node,omitempty"`
}

// PublishRequest writes a name record. Account selects the logged-in account when
// several are ("" with one). Kind is detected from held content when empty. WaitSeed
// names seed peers (cluster members) whose fresh complete report for exactly Target
// the call then waits for (W4); Timeout bounds that wait (default 10m).
type PublishRequest struct {
	Name     string   `json:"name"`
	Target   string   `json:"target"`
	Kind     string   `json:"kind,omitempty"`
	Note     string   `json:"note,omitempty"`
	Account  string   `json:"account,omitempty"`
	WaitSeed []string `json:"wait_seed,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

// Publication is a name write as the PDS answered it: what was written, not readiness.
type Publication struct {
	URI    string `json:"uri"`
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	// Record is the record CID the PDS returned; Rev the repository rev after the write.
	Record string `json:"record,omitempty"`
	Rev    string `json:"rev,omitempty"`
	// Dataset is the cluster dataset following this name on this node, when one exists.
	Dataset string `json:"dataset,omitempty"`
}

// PublishResult separates the publication from any seed wait (W4): `publication` says
// the record was written; `wait` says whether the named seeds hold the exact CID. Note
// states that publication alone is not readiness.
type PublishResult struct {
	Publication Publication `json:"publication"`
	Wait        *WaitResult `json:"wait,omitempty"`
	Note        string      `json:"note"`
}

// NameResolution is a verified name answer: the selected target and how fresh the
// evidence is. Source is "pds" (fetched now) or "cache"; Age the answer's age.
type NameResolution struct {
	URI       string `json:"uri"`
	DID       string `json:"did"`
	Handle    string `json:"handle,omitempty"`
	Name      string `json:"name"`
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size,omitempty"`
	Note      string `json:"note,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Rev       string `json:"rev"`
	Commit    string `json:"commit"`
	Record    string `json:"record"`
	Source    string `json:"source"`
	Age       string `json:"age"`
	// IdentityAge is how old the DID document used for verification was.
	IdentityAge string `json:"identity_age,omitempty"`
	// Held reports whether this node holds the target completely.
	Held bool `json:"held"`
}

// ResolveRequest selects freshness: MaxAge accepts a cached answer at most that old
// ("" uses the default), Fresh forces a PDS fetch, Offline answers only from the cache.
type ResolveRequest struct {
	MaxAge  string `json:"max_age,omitempty"`
	Fresh   bool   `json:"fresh,omitempty"`
	Offline bool   `json:"offline,omitempty"`
}

// NameList lists an account's names (each verified by its own proof).
type NameList struct {
	DID    string           `json:"did"`
	Handle string           `json:"handle,omitempty"`
	Names  []NameResolution `json:"names"`
}

// NodeRegisterRequest writes this node's io.ipfs.nu.node record into the account.
type NodeRegisterRequest struct {
	Account string `json:"account,omitempty"`
	Label   string `json:"label,omitempty"`
	Seed    bool   `json:"seed,omitempty"`
}

// NodeRecord is one io.ipfs.nu.node record of an account.
type NodeRecord struct {
	URI       string   `json:"uri"`
	DID       string   `json:"did"`
	Peer      string   `json:"peer"`
	Label     string   `json:"label,omitempty"`
	Roles     []string `json:"roles"`
	Relays    []string `json:"relays,omitempty"`
	DNSAddr   string   `json:"dnsaddr,omitempty"`
	CreatedAt string   `json:"created_at"`
	Rev       string   `json:"rev,omitempty"`
	// Self marks this node's own record.
	Self bool `json:"self,omitempty"`
}

// DeviceList lists an account's node records.
type DeviceList struct {
	DID     string       `json:"did"`
	Handle  string       `json:"handle,omitempty"`
	Devices []NodeRecord `json:"devices"`
}

// FollowRequest follows <handle-or-did>/<name> as a mirror-preset dataset with a name
// source (docs/follow.md). Dataset names the dataset ("" derives one from the name);
// Scope is "local" (default) or "cluster". Path, Lazy and MaxSize are the follow scope
// and quota; NoAnnounce follows unannounced; All (cluster scope) places every blob on
// every storage member (the seed layout).
type FollowRequest struct {
	Target     string `json:"target"`
	Dataset    string `json:"dataset,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Path       string `json:"path,omitempty"`
	Lazy       bool   `json:"lazy,omitempty"`
	MaxSize    int64  `json:"max_size,omitempty"`
	NoAnnounce bool   `json:"no_announce,omitempty"`
	All        bool   `json:"all,omitempty"`
	Replicas   int64  `json:"replicas,omitempty"`
	// Select follows a collection by local rules (a list of rule objects, the same
	// language as cluster collection rules; 0058): the catalog is held whole and only
	// the items the rules choose are fetched. Following the same name again with Select
	// replaces the rules.
	Select json.RawMessage `json:"select,omitempty"`
}

// FollowRoot is one of a follow's three roots (docs/follow.md "Lifecycle"): observed
// (newest verified proof), syncing (being acquired) and held (served).
type FollowRoot struct {
	Target string `json:"target"`
	Rev    string `json:"rev,omitempty"`
	Age    string `json:"age,omitempty"`
}

// Follow is a followed name's status: the dataset it lives in, its mode, the three
// roots, lag (held is behind observed), blockers and the dataset's content state.
type Follow struct {
	Dataset  string      `json:"dataset"`
	Scope    string      `json:"scope"`
	URI      string      `json:"uri"`
	DID      string      `json:"did"`
	Handle   string      `json:"handle,omitempty"`
	Name     string      `json:"name"`
	Mode     string      `json:"mode"` // full, scoped:<path>, lazy or select
	Observed *FollowRoot `json:"observed,omitempty"`
	Syncing  *FollowRoot `json:"syncing,omitempty"`
	Held     *FollowRoot `json:"held,omitempty"`
	// State is observed (nothing held yet), syncing, held, lagging (held behind
	// observed while syncing), paused (quota/space/scope), ended (verified deletion) or
	// error; Detail explains it.
	State    string   `json:"state"`
	Detail   string   `json:"detail,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	// Ended is set when a verified deletion ended the follow; its pins stay (orphans).
	Ended   *FollowRoot  `json:"ended,omitempty"`
	Orphans []string     `json:"orphans,omitempty"`
	Content ContentState `json:"content"`
	// Selection is a select follow's catalog and holdings summary (0058).
	Selection *FollowSelection `json:"selection,omitempty"`
	// Refreshed is when the name was last re-resolved; Error the last refresh error.
	Refreshed time.Time `json:"refreshed,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// FollowSelection summarises a select follow (docs/collections.md "Following
// collections"): the catalog head last synced and whether it is held whole, the facets
// the publisher declared, the rule count, and the items by state: Selected is what the
// rules choose now (held + fetching + paused), Paused the matches the budgets leave out
// (Shortfall their bytes).
type FollowSelection struct {
	Catalog       string               `json:"catalog,omitempty"`
	CatalogSynced bool                 `json:"catalog_synced"`
	Facets        map[string]FacetDecl `json:"facets,omitempty"`
	Rules         int                  `json:"rules"`
	Items         int                  `json:"items"`
	Selected      int                  `json:"selected"`
	Held          int                  `json:"held"`
	HeldBytes     uint64               `json:"held_bytes"`
	Fetching      int                  `json:"fetching"`
	Paused        int                  `json:"paused"`
	Shortfall     uint64               `json:"shortfall,omitempty"`
	Warnings      []string             `json:"warnings,omitempty"`
}

// FollowRefreshRequest re-resolves one follow (or every follow with Dataset "").
type FollowRefreshRequest struct {
	Dataset string `json:"dataset,omitempty"`
}
