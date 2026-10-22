package apiv0

import "time"

// Repair, handoff, audits and release (docs/cluster.md "Sync, handoff and repair";
// docs/operations.md S1–S8, E9; backlog 0048).

// Alert is a machine-readable alert condition with the recovery action.
type Alert struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	Action  string `json:"action,omitempty"`
	Peer    string `json:"peer,omitempty"`
	Dataset string `json:"dataset,omitempty"`
}

// Alert codes.
const (
	AlertNoRepairAuthority = "no_repair_authority"
	AlertMemberUnavailable = "member_unavailable"
	AlertMemberSilent      = "member_silent"
	AlertAuditFailed       = "audit_failed"
	AlertScrubFailed       = "scrub_failed"
	AlertDegraded          = "degraded"
	AlertUnknownTooLong    = "unknown_too_long"
)

// RepairInfo is the repair-authority state as this node sees it: Enabled says this node
// is one; Online that some authority (this node or a connected member announcing one) is.
type RepairInfo struct {
	Enabled     bool     `json:"enabled"`
	Online      bool     `json:"online"`
	Authorities []string `json:"authorities"`
	DownAfterMS int64    `json:"down_after_ms,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

// MemberSetRequest edits a member record; empty fields keep their value.
type MemberSetRequest struct {
	Peer       string            `json:"peer"`
	State      string            `json:"state,omitempty"` // active, draining, unavailable
	Capacity   int64             `json:"capacity,omitempty"`
	StoreLimit int64             `json:"store_limit,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Label      string            `json:"label,omitempty"`
}

// MemberResult is a member record write.
type MemberResult struct {
	Member   MemberInfo    `json:"member"`
	Metadata MetadataState `json:"metadata"`
	Warnings []string      `json:"warnings,omitempty"`
}

// Unconfirmed is a dataset's content whose copies a removal would leave unconfirmed (S6):
// units below k confirmed owner copies in the current epoch, with the owners missing.
type Unconfirmed struct {
	Dataset string         `json:"dataset"`
	Target  string         `json:"target,omitempty"`
	Units   int64          `json:"units"`
	Unit    string         `json:"unit"`
	Missing []MissingOwner `json:"missing"`
	Detail  string         `json:"detail,omitempty"`
}

// MemberRemoval is the result (or dry-run report) of `member rm` (S6).
type MemberRemoval struct {
	Peer    string `json:"peer"`
	State   string `json:"state"`
	DryRun  bool   `json:"dry_run"`
	Safe    bool   `json:"safe"`
	Removed bool   `json:"removed"`
	Forced  bool   `json:"forced,omitempty"`
	// Unconfirmed lists what lacks S2 evidence; with Force it is what the removal
	// left unconfirmed.
	Unconfirmed []Unconfirmed  `json:"unconfirmed"`
	Reasons     []string       `json:"reasons,omitempty"`
	Metadata    *MetadataState `json:"metadata,omitempty"`
}

// KeyRequest creates a key on this node (no DID) or grants a role to a DID.
type KeyRequest struct {
	DID     string `json:"did,omitempty"`
	Role    string `json:"role,omitempty"`    // admin or writer
	Dataset string `json:"dataset,omitempty"` // writer: the dataset it may write
}

// KeyResult is a key operation's result.
type KeyResult struct {
	DID      string         `json:"did"`
	Role     string         `json:"role,omitempty"`
	Held     bool           `json:"held,omitempty"`
	Revoked  bool           `json:"revoked,omitempty"`
	Metadata *MetadataState `json:"metadata,omitempty"`
}

// ReleaseRequest runs (or with DryRun reports) safe release for one dataset.
type ReleaseRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
}

// ReleaseCandidate is a release candidate with its S2 evidence or shortfall: Kind
// "version" (an earlier target this member keeps pinned) or "surplus" (blobs it holds
// but no longer owns, grouped by owner set).
type ReleaseCandidate struct {
	Kind      string         `json:"kind"`
	CID       string         `json:"cid,omitempty"`
	Blobs     int64          `json:"blobs"`
	Bytes     int64          `json:"bytes"`
	OK        bool           `json:"ok"`
	Confirmed []string       `json:"confirmed"`
	Shortfall []MissingOwner `json:"shortfall"`
	// PendingUntil is when an automatic release of an OK surplus group happens
	// (dataset.remove_grace after it first passed).
	PendingUntil *time.Time `json:"pending_until,omitempty"`
}

// ReleaseReport is the per-member release decision for one dataset (S1–S4, S8).
type ReleaseReport struct {
	Dataset  string   `json:"dataset"`
	Auto     bool     `json:"auto"`
	DryRun   bool     `json:"dry_run"`
	Target   string   `json:"target,omitempty"`
	Live     string   `json:"live,omitempty"`
	Epoch    string   `json:"epoch,omitempty"`
	Policy   string   `json:"policy,omitempty"`
	Deferred []string `json:"deferred"`
	// HeadConfirmed says every blob of the head and live set has k confirmed owner
	// copies (the evidence that releases earlier versions); HeadShortfall names the
	// owners missing otherwise.
	HeadConfirmed bool               `json:"head_confirmed"`
	HeadShortfall []MissingOwner     `json:"head_shortfall,omitempty"`
	Candidates    []ReleaseCandidate `json:"candidates"`
	// Released lists the versions unpinned and the surplus blobs dropped by this call.
	Released      []string `json:"released,omitempty"`
	ReleasedBlobs int64    `json:"released_blobs,omitempty"`
	ReleasedBytes int64    `json:"released_bytes,omitempty"`
}

// AuditRequest runs an audit now: Samples remote challenges per storage member (Peer:
// only that one) and a local scrub of Scrub sampled objects (Full: every held object).
type AuditRequest struct {
	Peer    string `json:"peer,omitempty"`
	Samples int    `json:"samples,omitempty"`
	Scrub   int    `json:"scrub,omitempty"`
	Full    bool   `json:"full,omitempty"`
}

// Challenge is one audit challenge: a member served one chunk group of a blob it owns,
// verified against the CID (E9: not a completion report).
type Challenge struct {
	Peer    string `json:"peer"`
	Dataset string `json:"dataset"`
	CID     string `json:"cid"`
	Group   uint64 `json:"group"`
	OK      bool   `json:"ok"`
	MS      int64  `json:"ms"`
	Error   string `json:"error,omitempty"`
}

// AuditPeer summarises audits of one member.
type AuditPeer struct {
	Peer        string     `json:"peer"`
	Passed      int        `json:"passed"`
	Failed      int        `json:"failed"`
	Consecutive int        `json:"consecutive_failures"`
	Last        *time.Time `json:"last,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// ScrubReport is a local verification pass over held objects.
type ScrubReport struct {
	At      time.Time `json:"at"`
	Full    bool      `json:"full"`
	Checked int       `json:"checked"`
	Bytes   int64     `json:"bytes"`
	Corrupt []string  `json:"corrupt"`
}

// AuditReport is the result of an audit run.
type AuditReport struct {
	Challenges []Challenge  `json:"challenges"`
	Scrub      *ScrubReport `json:"scrub,omitempty"`
	// Note restates E9: audits never count as completion reports.
	Note string `json:"note"`
}
