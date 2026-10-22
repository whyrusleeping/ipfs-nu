package apiv0

import (
	"encoding/json"
	"time"
)

// atproto records, repository snapshots and CAR import/export (0053; docs/dasl.md "Using
// atproto data").

// AtRecord is a verified atproto record: the result of `nu get at://<did>/<collection>/<rkey>`
// and of `cat` on a record of a held snapshot.
type AtRecord struct {
	// URI is the canonical at://<did>/<collection>/<rkey>.
	URI    string `json:"uri"`
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	// PDS is the server the proof came from ("" when served from a held snapshot).
	PDS        string `json:"pds,omitempty"`
	Collection string `json:"collection"`
	RKey       string `json:"rkey"`
	// CID is the record block; Commit and Rev the signed commit that proves it.
	CID    string `json:"cid"`
	Commit string `json:"commit"`
	Rev    string `json:"rev"`
	// Value is the record as atproto-style JSON ($link, $bytes).
	Value json.RawMessage `json:"value"`
	// Source is "pds" (fresh proof) or "snapshot" (held, verified at import time).
	Source string `json:"source"`
	// IdentityAge is how old the cached DID document used for verification was.
	IdentityAge string `json:"identity_age,omitempty"`
	// DataModel notes a record that breaks the atproto data model (kept byte-exactly).
	DataModel string `json:"data_model,omitempty"`
}

// AtSnapshotRequest starts a repository snapshot operation: fetch getRepo for the
// account, verify it completely, store and pin it under the atrepo profile.
type AtSnapshotRequest struct {
	// Target is at://<handle-or-did>.
	Target string `json:"target"`
	// Blobs also fetches the blobs the records reference (SHA-256 raw CIDs, ≤ 8 MiB each)
	// and pins under atrepo+blobs; oversized blobs are skipped and counted.
	Blobs bool `json:"blobs,omitempty"`
}

// AtSnapshot is the result of a snapshot operation, and one row of held snapshots.
type AtSnapshot struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	PDS    string `json:"pds,omitempty"`
	Commit string `json:"commit"`
	Rev    string `json:"rev"`
	// Records and Nodes count the verified records and MST nodes; Bytes is the CAR size.
	Records int   `json:"records"`
	Nodes   int   `json:"nodes"`
	Bytes   int64 `json:"bytes,omitempty"`
	// NonConforming lists records that break the atproto data model (kept anyway).
	NonConforming []string `json:"non_conforming,omitempty"`
	// Profile is the pin's walk profile: atrepo or atrepo+blobs.
	Profile string `json:"profile"`
	// Blob counts (with Blobs): fetched, skipped as oversized (> 8 MiB), failed.
	Blobs          int `json:"blobs,omitempty"`
	BlobsOversized int `json:"blobs_oversized,omitempty"`
	BlobsFailed    int `json:"blobs_failed,omitempty"`
}

// CAR walk profiles (docs/dasl.md "Walk profiles").
const (
	ProfileNuTree = "nu-tree"
	ProfileAtrepo = "atrepo"
	ProfileDRISL  = "drisl"
)

// CarImportRequest starts a CAR import operation. The daemon reads Path itself (an
// absolute path, like add). Profile is nu-tree, atrepo or drisl; "" detects it from the
// root block (a v3 commit → atrepo, a tree header → nu-tree, else drisl).
type CarImportRequest struct {
	Path    string `json:"path"`
	Profile string `json:"profile,omitempty"`
	// Key is a did:key or DID-document publicKeyMultibase that must have signed an
	// atrepo commit; without it the commit's DID is resolved online unless Offline is set.
	Key     string `json:"key,omitempty"`
	Offline bool   `json:"offline,omitempty"`
	// Pin pins the root under the profile after verification (the CLI default).
	Pin bool `json:"pin,omitempty"`
}

// CarImport is the result of a CAR import.
type CarImport struct {
	Root    string `json:"root"`
	Profile string `json:"profile"`
	// Blocks and Bytes count the CAR's distinct blocks; Unreferenced the blocks nothing
	// in the CAR links to (kept, but not pinned by a structural profile).
	Blocks       int   `json:"blocks"`
	Bytes        int64 `json:"bytes"`
	Unreferenced int   `json:"unreferenced,omitempty"`
	// Objects counts the objects reachable from the root under the profile.
	Objects int  `json:"objects"`
	Pinned  bool `json:"pinned"`
	// atrepo fields.
	DID           string   `json:"did,omitempty"`
	Rev           string   `json:"rev,omitempty"`
	Records       int      `json:"records,omitempty"`
	NonConforming []string `json:"non_conforming,omitempty"`
	// Signature is "verified" (with the key named in SignedBy) or "unverified: <why>" when
	// no key was given and the identity could not be resolved (offline import).
	Signature string `json:"signature,omitempty"`
	SignedBy  string `json:"signed_by,omitempty"`
}

// AtrepoSource is an atproto-account dataset's source state (0054, D21): the account,
// its scope, the redistribution rule in force and the admin's update bookkeeping
// (meaningful on the node that fetches; storage-only members report the head only).
type AtrepoSource struct {
	DID    string   `json:"did"`
	Handle string   `json:"handle,omitempty"`
	NSIDs  []string `json:"nsids,omitempty"`
	Blobs  bool     `json:"blobs,omitempty"`
	// Own says the operator controls the account; PublishHistory is the recorded
	// override; Redistribution is the rule in force: "unrestricted" (own account or
	// override), "current-only" (third-party, active) or "private" (account inactive).
	Own            bool   `json:"own,omitempty"`
	PublishHistory bool   `json:"publish_history,omitempty"`
	Redistribution string `json:"redistribution"`
	// Status is the verified account status ("active", deactivated, takendown,
	// suspended, deleted); Rev the held rev, PDSRev the PDS's current rev when known
	// (lag = the two differ); Refreshed/Error/Mode (since, full, status) describe the
	// last update attempt.
	Status    string    `json:"status"`
	Rev       string    `json:"rev,omitempty"`
	PDSRev    string    `json:"pds_rev,omitempty"`
	Refreshed time.Time `json:"refreshed,omitzero"`
	Error     string    `json:"error,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	// Blob bookkeeping of this node's fetches: fetched, oversized (> 8 MiB, skipped)
	// and failed blobs.
	BlobsFetched   int `json:"blobs_fetched,omitempty"`
	BlobsOversized int `json:"blobs_oversized,omitempty"`
	BlobsFailed    int `json:"blobs_failed,omitempty"`
}
