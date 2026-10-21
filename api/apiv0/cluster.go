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

// ContentState is the content side of a status. Placement and readiness evidence are
// not implemented yet, so State is "not_tracked" (or "none" with no head).
type ContentState struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
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
	Name     string   `json:"name"`
	Replicas int64    `json:"replicas,omitempty"` // collections default to 0 (no guaranteed copies)
	Writers  []string `json:"writers,omitempty"`
	Note     string   `json:"note,omitempty"`
	// Collection makes the dataset a collection (D22) with Facets declared over item
	// metadata, Merge "union" (default) or "lww", and Expire "<time facet>=<duration>".
	Collection bool                 `json:"collection,omitempty"`
	Facets     map[string]FacetDecl `json:"facets,omitempty"`
	Merge      string               `json:"merge,omitempty"`
	Expire     string               `json:"expire,omitempty"`
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
	Name     string        `json:"name"`
	Cluster  string        `json:"cluster"` // "local" or the networked cluster's ID
	Source   string        `json:"source"`
	Replicas int64         `json:"replicas"`
	Writers  []string      `json:"writers,omitempty"`
	Note     string        `json:"note,omitempty"`
	// Collection carries a collection's facets and merge mode.
	Collection *DatasetCollection `json:"collection,omitempty"`
	Signer     string             `json:"signer"`
	Head     *DatasetHead  `json:"head,omitempty"`
	Heads    []DatasetHead `json:"heads,omitempty"` // per-signer heads (union-head input)
	Metadata MetadataState `json:"metadata"`
	Content  ContentState  `json:"content"`
}

// DatasetHead is a head record by one signer.
type DatasetHead struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Signer string `json:"signer"`
	HLC    string `json:"hlc"`
}

// DatasetCollection is a dataset's collection policy.
type DatasetCollection struct {
	Facets map[string]FacetDecl `json:"facets,omitempty"`
	Merge  string               `json:"merge"`
	Expire string               `json:"expire,omitempty"`
}
