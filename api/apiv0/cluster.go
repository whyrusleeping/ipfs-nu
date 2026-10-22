package apiv0

import "time"

// Cluster API types (docs/cluster.md D20; docs/api.md "Clusters").

// ClusterInitRequest founds a networked cluster with this node as root holder, first
// admin and first storage member.
type ClusterInitRequest struct {
	Name string `json:"name"`
	Mode string `json:"mode"` // public, private (default) or isolated
}

// ClusterInviteRequest creates an invite. With Peer the invite is pre-authorized (the
// member record is written now, offline); without it a one-time secret is issued.
type ClusterInviteRequest struct {
	Role     string            `json:"role,omitempty"` // storage (default) or client
	Tags     map[string]string `json:"tags,omitempty"`
	Capacity int64             `json:"capacity,omitempty"`
	Peer     string            `json:"peer,omitempty"`
	Expires  string            `json:"expires,omitempty"` // duration, default 24h
}

// ClusterInvite is a created invite.
type ClusterInvite struct {
	Token    string        `json:"token"`
	Peer     string        `json:"peer,omitempty"`
	InviteID string        `json:"invite_id,omitempty"`
	Expires  time.Time     `json:"expires"`
	Metadata MetadataState `json:"metadata"`
}

// ClusterJoinRequest joins a cluster with an invite token.
type ClusterJoinRequest struct {
	Token string `json:"token"`
}

// ClusterMemberRemoveRequest removes (revokes) a member.
type ClusterMemberRemoveRequest struct {
	Peer string `json:"peer"`
}

// MetadataState reports acknowledgment of signed cluster state. It says nothing about
// content, which ContentState reports separately.
type MetadataState struct {
	// Signer, Commit and Rev identify the state commit that carries the write.
	Signer string `json:"signer,omitempty"`
	Commit string `json:"commit,omitempty"`
	Rev    string `json:"rev,omitempty"`
	// AckedBy lists the storage members known to hold the commit (base36 PeerIds).
	AckedBy []string `json:"acked_by"`
	// Required is how many acknowledgments the write waited for; Acknowledged reports
	// whether they arrived before the wait ended.
	Required     int  `json:"required"`
	Acknowledged bool `json:"acknowledged"`
}

// ContentState is the content side of a dataset status (docs/operations.md "Readiness
// and status"): State is ready, syncing, unknown, degraded, policy_unsatisfiable, paused
// or error ("none" without a head); any state other than "ready" is not ready. The W3
// fields say why: the exact Target, the report Mode, placement Epoch and Policy hash it
// is evaluated for, the owners Confirmed by fresh complete reports out of Required, the
// owners still Missing with their reason (stale, incomplete, epoch, absent), Extra fresh
// complete reports from non-owners (never counted), the age of the oldest counted report
// and the Unmet requirements. A head write's Metadata and its Content are separate
// results (E10).
type ContentState struct {
	State     string         `json:"state"`
	Detail    string         `json:"detail,omitempty"`
	Target    string         `json:"target,omitempty"`
	Mode      string         `json:"mode,omitempty"`
	Epoch     string         `json:"epoch,omitempty"`
	Policy    string         `json:"policy,omitempty"`
	Confirmed []Confirmation `json:"confirmed"`
	Required  int            `json:"required"`
	Missing   []MissingOwner `json:"missing"`
	Extra     []Confirmation `json:"extra"`
	// Copies summarises confirmed copies per unit (E7): units counted, those below
	// Required, and the fewest confirmed copies of any unit.
	Copies      *CopyCounts `json:"copies,omitempty"`
	EvidenceAge int64       `json:"evidence_age_ms"`
	Unmet       []string    `json:"unmet"`
	// Live is the live-set identity an archive-wide evaluation is bound to (E1).
	Live string `json:"live,omitempty"`
	// LastReady is the last time this node saw Target ready (E8: kept beside a later
	// unknown or degraded result).
	LastReady *LastReady `json:"last_ready,omitempty"`
	// Local is this node's own sync of the dataset; Release its protected versions.
	Local   *LocalSync    `json:"local,omitempty"`
	Release *ReleaseState `json:"release,omitempty"`
}

// CopyCounts is the per-unit copy summary of a readiness evaluation; Unit is "blob", or
// "item" for collections.
type CopyCounts struct {
	Unit  string `json:"unit"`
	Units int64  `json:"units"`
	Below int64  `json:"below"`
	Min   int    `json:"min"`
}

// Confirmation is one counted (or extra) fresh complete report.
type Confirmation struct {
	Peer  string `json:"peer"`
	Seq   int64  `json:"seq"`
	AgeMS int64  `json:"age_ms"`
}

// MissingOwner is an owner without a counted report: stale, incomplete, epoch (bound to
// another epoch, policy or mode) or absent (no report for this target).
type MissingOwner struct {
	Peer   string `json:"peer"`
	Reason string `json:"reason"`
}

// LastReady is the last ready evaluation of a target on this node.
type LastReady struct {
	Target        string    `json:"target"`
	Epoch         string    `json:"epoch"`
	EvidenceAgeMS int64     `json:"evidence_age_ms"`
	At            time.Time `json:"at"`
}

// LocalSync is this member's own sync of a dataset: the target it works toward (under
// first-held protection a staged head; Observed is then the head it only observes),
// whether it owns blobs there (mode owned) or only holds them (full), its completeness,
// and when paused the reason and bytes needed (S7).
type LocalSync struct {
	Target   string `json:"target"`
	Observed string `json:"observed,omitempty"`
	Mode     string `json:"mode"`
	State    string `json:"state"`
	Complete bool   `json:"complete"`
	Owned    Counts `json:"owned"`
	Missing  Counts `json:"missing"`
	Reason   string `json:"reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Need     int64  `json:"need,omitempty"`
}

// Counts is a number of blobs and their bytes.
type Counts struct {
	Blobs int64 `json:"blobs"`
	Bytes int64 `json:"bytes"`
}

// ReleaseState lists the versions this node keeps pinned for a dataset. Release is
// automatic only once release.auto is enabled (off by default: candidates are listed,
// nothing is released).
type ReleaseState struct {
	Auto       bool     `json:"auto"`
	Ready      string   `json:"ready,omitempty"` // last policy-ready head
	Held       string   `json:"held,omitempty"`  // last fully held target
	Staged     []string `json:"staged,omitempty"`
	Candidates []string `json:"candidates,omitempty"` // retained, no longer protected
}

// DatasetUpdateRequest writes a dataset's head (a new target). Kind is tree, file or
// blob; empty detects it from held content.
type DatasetUpdateRequest struct {
	Target string `json:"target"`
	Kind   string `json:"kind,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Wait conditions (docs/operations.md W2).
const (
	WaitMetadata = "metadata"
	WaitHeld     = "held"
	WaitReady    = "ready"
	WaitArchive  = "archive-ready"
)

// DatasetWaitRequest waits for one condition on an exact target (W1): Holders for held,
// Acks storage members for metadata (default 1), Mode for held (empty: any), Timeout a
// duration (default 30s for metadata, 10m otherwise; "0" waits indefinitely).
type DatasetWaitRequest struct {
	Target    string   `json:"target"`
	Condition string   `json:"condition"`
	Holders   []string `json:"holders,omitempty"`
	Acks      int      `json:"acks,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Timeout   string   `json:"timeout,omitempty"`
}

// WaitResult is a wait's outcome (W3): the evaluation at that instant, the condition,
// whether it is satisfied and, when the head moved during the wait, the new head. An
// unsatisfied wait is also an error (timeout, or failed when the condition cannot be met
// without a state change) whose details carry this result under "result".
type WaitResult struct {
	ContentState
	Condition    string        `json:"condition"`
	Satisfied    bool          `json:"satisfied"`
	SupersededBy string        `json:"superseded_by,omitempty"`
	Metadata     *MetadataWait `json:"metadata,omitempty"`
}

// MetadataWait is the evidence of a metadata wait: the storage members holding the state
// commit that carries the head record.
type MetadataWait struct {
	Signer  string   `json:"signer"`
	Rev     string   `json:"rev"`
	AckedBy []string `json:"acked_by"`
	Acks    int      `json:"acks"`
}

// ClusterStatus describes the node's clusters.
type ClusterStatus struct {
	Local   *ClusterInfo `json:"local,omitempty"`
	Network *ClusterInfo `json:"network,omitempty"`
}

// ClusterInfo is one cluster as this node sees it.
type ClusterInfo struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Mode     string        `json:"mode"`
	Self     string        `json:"self"`
	Member   bool          `json:"member"`
	Keys     []ClusterKey  `json:"keys"`
	Members  []MemberInfo  `json:"members"`
	Repos    []RepoInfo    `json:"repos"`
	Datasets int           `json:"datasets"`
	Invites  int           `json:"live_invites"`
	Ignored  int           `json:"ignored_records"`
	HeldBack int           `json:"held_back_records"`
	Boot     []string      `json:"boot,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Metadata MetadataState `json:"metadata"`
}

// ClusterKey is an authorized key.
type ClusterKey struct {
	DID     string `json:"did"`
	Role    string `json:"role"`
	Revoked bool   `json:"revoked,omitempty"`
	Held    bool   `json:"held,omitempty"` // this node holds its private key
}

// MemberInfo is one member with its liveness as seen from this node.
type MemberInfo struct {
	Peer          string            `json:"peer"`
	Roles         []string          `json:"roles"`
	State         string            `json:"state"`
	Tags          map[string]string `json:"tags,omitempty"`
	Addrs         []string          `json:"addrs,omitempty"`
	Self          bool              `json:"self,omitempty"`
	Connected     bool              `json:"connected"`
	LastHeartbeat *time.Time        `json:"last_heartbeat,omitempty"`
	Free          int64             `json:"free,omitempty"`
	Used          int64             `json:"used,omitempty"`
	SkewMS        int64             `json:"skew_ms,omitempty"`
}

// RepoInfo is one held state repository.
type RepoInfo struct {
	DID    string `json:"did"`
	Role   string `json:"role,omitempty"`
	Rev    string `json:"rev,omitempty"`
	Commit string `json:"commit,omitempty"`
	Status string `json:"status"`
}

// DatasetCreateRequest creates a dataset record.
type DatasetCreateRequest struct {
	Name     string `json:"name"`
	Replicas int64  `json:"replicas,omitempty"` // collections default to 0 (no guaranteed copies)
	// All places every blob on every eligible storage member (replicas: all).
	All bool `json:"all,omitempty"`
	// Where restricts placement to storage members whose tags match (tag → allowed
	// values); Spread requires distinct values of a tag among each blob's owners.
	Where   map[string][]string `json:"where,omitempty"`
	Spread  string              `json:"spread,omitempty"`
	Writers []string            `json:"writers,omitempty"`
	Note    string              `json:"note,omitempty"`
	// Collection makes the dataset a collection (D22) with Facets declared over item
	// metadata, Merge "union" (default) or "lww", and Expire "<time facet>=<duration>".
	Collection bool                 `json:"collection,omitempty"`
	Facets     map[string]FacetDecl `json:"facets,omitempty"`
	Merge      string               `json:"merge,omitempty"`
	Expire     string               `json:"expire,omitempty"`
	// Ingest is a collection's push policy (docs/collections.md "Ingest").
	Ingest *DatasetIngest `json:"ingest,omitempty"`
	// Presets (0047): Archive enables History (default schedule unless given), Mirror
	// follows current state; SourcePath is a writer-local absolute directory that an
	// update without a target re-adds.
	Archive    bool           `json:"archive,omitempty"`
	Mirror     bool           `json:"mirror,omitempty"`
	History    *HistoryPolicy `json:"history,omitempty"`
	SourcePath string         `json:"source_path,omitempty"`
	// SourceName is the followed name of a name-sourced dataset (0045).
	SourceName string `json:"source_name,omitempty"`
	// SourceAt archives an atproto account (0054): at://<handle-or-did>; NSIDs scopes
	// the collections (all when empty), Blobs selects its blobs, PublishHistory is the
	// explicit override that lets retained third-party history leave the cluster.
	SourceAt       string   `json:"source_at,omitempty"`
	NSIDs          []string `json:"nsids,omitempty"`
	Blobs          bool     `json:"blobs,omitempty"`
	PublishHistory bool     `json:"publish_history,omitempty"`
}

// DatasetIngest is a collection's push policy: Hosts are the exact host names receiving
// members may fetch item URLs from in private mode (and, when set, in public mode);
// MaxBytes, Attempts and Timeout bound each URL file (0/"" take the defaults: 1 TiB, 6
// attempts, 2h per attempt).
type DatasetIngest struct {
	Hosts    []string `json:"hosts,omitempty"`
	MaxBytes int64    `json:"max_bytes,omitempty"`
	Attempts int64    `json:"attempts,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

// FacetDecl declares a collection facet: a JSON Pointer into the metadata record's JSON
// form and a type (time, number, string or bbox).
type FacetDecl struct {
	Ptr  string `json:"ptr" toml:"ptr"`
	Type string `json:"type" toml:"type"`
}

// Dataset is a dataset's merged record and heads, with metadata and content status
// reported separately.
type Dataset struct {
	Name     string `json:"name"`
	Cluster  string `json:"cluster"` // "local" or the networked cluster's ID
	Source   string `json:"source"`
	Replicas int64  `json:"replicas"`
	All      bool   `json:"all,omitempty"`
	// Where and Spread are the placement policy's tag filter and spread tag.
	Where    map[string][]string `json:"where,omitempty"`
	Spread   string              `json:"spread,omitempty"`
	Writers  []string            `json:"writers,omitempty"`
	Note     string              `json:"note,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
	// Collection carries a collection's facets and merge mode.
	Collection *DatasetCollection `json:"collection,omitempty"`
	Signer     string             `json:"signer"`
	Head       *DatasetHead       `json:"head,omitempty"`
	Heads      []DatasetHead      `json:"heads,omitempty"` // per-signer heads (union-head input)
	Metadata   MetadataState      `json:"metadata"`
	Content    ContentState       `json:"content"`
	// History and SourcePath are set for archives and path-sourced datasets; Archive is
	// archive-wide readiness over the live set (E8); Version is the version a head write
	// or rollback appended.
	History    *HistoryPolicy `json:"history,omitempty"`
	SourcePath string         `json:"source_path,omitempty"`
	// SourceName is the followed name of a name-sourced dataset (0045).
	SourceName string        `json:"source_name,omitempty"`
	Archive    *ContentState `json:"archive,omitempty"`
	Version    *VersionInfo  `json:"version,omitempty"`
	// Atrepo is an atproto-account source's state (0054).
	Atrepo *AtrepoSource `json:"atrepo,omitempty"`
}

// DatasetHead is a head record by one signer.
type DatasetHead struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Signer string `json:"signer"`
	HLC    string `json:"hlc"`
	// Rev is an observed head's source commit rev (names, atrepo); Status an atrepo
	// head's account status ("" active).
	Rev    string `json:"rev,omitempty"`
	Status string `json:"status,omitempty"`
}

// DatasetCollection is a dataset's collection policy.
type DatasetCollection struct {
	Facets map[string]FacetDecl `json:"facets,omitempty"`
	Merge  string               `json:"merge"`
	Expire string               `json:"expire,omitempty"`
	Ingest *DatasetIngest       `json:"ingest,omitempty"`
}
