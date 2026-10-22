package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Record collections (docs/cluster.md "Cluster state").
const (
	NSGenesis = "io.ipfs.nu.cluster.genesis"
	NSKey     = "io.ipfs.nu.cluster.key"
	NSMember  = "io.ipfs.nu.cluster.member"
	NSInvite  = "io.ipfs.nu.cluster.invite"
	NSDataset = "io.ipfs.nu.cluster.dataset"
	NSHead    = "io.ipfs.nu.cluster.head"
	NSHistory = "io.ipfs.nu.cluster.history"
	NSVersion = "io.ipfs.nu.cluster.version"
	NSDrop    = "io.ipfs.nu.cluster.drop"
	NSConfig  = "io.ipfs.nu.cluster.config"
)

// Modes.
const (
	ModePublic   = "public"
	ModePrivate  = "private"
	ModeIsolated = "isolated"
	ModeLocal    = "local"
)

// Key roles, member roles and member states.
const (
	RoleRoot    = "root"
	RoleAdmin   = "admin"
	RoleWriter  = "writer"
	RoleStorage = "storage"
	RoleClient  = "client"

	StateActive      = "active"
	StateDraining    = "draining"
	StateUnavailable = "unavailable"
	StateRemoved     = "removed"
)

// MaxRecord bounds one cluster record block.
const MaxRecord = 16 << 10

// Genesis is the cluster's founding record; the cluster ID is its CID.
type Genesis struct {
	Name      string `drisl:"name" json:"name"`
	Mode      string `drisl:"mode" json:"mode"`
	RootKey   string `drisl:"rootKey" json:"rootKey"`
	CreatedAt string `drisl:"createdAt" json:"createdAt"`
}

func (Genesis) DRISLType() string { return NSGenesis }

// ID returns the cluster ID: the CID of the DRISL genesis record.
func (g Genesis) ID() (cid.CID, error) {
	c, _, err := drisl.Sum(g)
	return c, err
}

// Validate checks the genesis fields.
func (g Genesis) Validate() error {
	switch g.Mode {
	case ModePublic, ModePrivate, ModeIsolated, ModeLocal:
	default:
		return fmt.Errorf("cluster: unknown mode %q", g.Mode)
	}
	if g.Name == "" || len(g.Name) > 64 {
		return errors.New("cluster: genesis name must have 1 to 64 bytes")
	}
	if _, err := parseP256(g.RootKey); err != nil {
		return fmt.Errorf("cluster: genesis rootKey: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, g.CreatedAt); err != nil {
		return fmt.Errorf("cluster: genesis createdAt: %w", err)
	}
	return nil
}

// Key grants a role to a did:key.
type Key struct {
	Role    string      `drisl:"role"`
	Revoked *Revocation `drisl:"revoked,omitempty"`
	HLC     string      `drisl:"hlc"`
}

func (Key) DRISLType() string { return NSKey }

// Revocation freezes a key's repository at its last accepted commit.
type Revocation struct {
	After cid.CID `drisl:"after"`
	Rev   string  `drisl:"rev"`
}

// Member admits a node.
type Member struct {
	Roles      []string          `drisl:"roles"`
	Capacity   int64             `drisl:"capacity"`
	StoreLimit int64             `drisl:"store_limit,omitempty"`
	Tags       map[string]string `drisl:"tags,omitempty"`
	Addrs      []string          `drisl:"addrs,omitempty"`
	Label      string            `drisl:"label,omitempty"`
	State      string            `drisl:"state"`
	HLC        string            `drisl:"hlc"`
}

func (Member) DRISLType() string { return NSMember }

// Invite is an outstanding secret invite.
type Invite struct {
	ProofKey []byte            `drisl:"proofKey"`
	Roles    []string          `drisl:"roles"`
	Tags     map[string]string `drisl:"tags,omitempty"`
	Capacity int64             `drisl:"capacity,omitempty"`
	Expires  string            `drisl:"expires"`
	HLC      string            `drisl:"hlc"`
}

func (Invite) DRISLType() string { return NSInvite }

// ExpiresAt parses Expires (validated records always parse).
func (i *Invite) ExpiresAt() time.Time {
	t, _ := time.Parse(time.RFC3339, i.Expires)
	return t
}

// Dataset is a named pinset entry.
type Dataset struct {
	Source  Source   `drisl:"source"`
	Policy  Policy   `drisl:"policy"`
	Writers []string `drisl:"writers,omitempty"`
	// Collection makes the dataset a collection (D22).
	Collection *CollectionPolicy `drisl:"collection,omitempty"`
	Note       string            `drisl:"note,omitempty"`
	HLC        string            `drisl:"hlc"`

	policyHash string // set when decoded from a record
}

func (Dataset) DRISLType() string { return NSDataset }

// PolicyHash is the hex SHA-256 of the DRISL encoding of the record's `policy` map as
// signed (unknown fields included), the `policy` binding of completion reports (E3).
// It is empty for a Dataset that was not decoded from a record.
func (d *Dataset) PolicyHash() string { return d.policyHash }

// Merge modes of a collection's head.
const (
	MergeUnion = "union"
	MergeLWW   = "lww"
)

// CollectionPolicy is a dataset's `collection` field: facets declared over item
// metadata, the head merge mode (default union) and an optional expiry rule.
type CollectionPolicy struct {
	Facets map[string]Facet `drisl:"facets,omitempty"`
	Merge  string           `drisl:"merge,omitempty"`
	Expire *Expire          `drisl:"expire,omitempty"`
}

// MergeMode returns the effective merge mode.
func (c *CollectionPolicy) MergeMode() string {
	if c.Merge == "" {
		return MergeUnion
	}
	return c.Merge
}

// Facet declares a typed value extracted from item metadata by JSON Pointer.
type Facet struct {
	Ptr  string `drisl:"ptr" json:"ptr" toml:"ptr"`
	Type string `drisl:"type" json:"type" toml:"type"`
}

// Expire makes every writer prune items whose time facet is older than After (a
// duration such as "400d") from its head.
type Expire struct {
	Facet string `drisl:"facet" json:"facet"`
	After string `drisl:"after" json:"after"`
}

// Drop removes items from a union collection: the merged head leaves out every path at or
// below one of Paths.
type Drop struct {
	Paths []string `drisl:"paths"`
	HLC   string   `drisl:"hlc"`
}

func (Drop) DRISLType() string { return NSDrop }

// MaxDropPaths bounds a drop record.
const MaxDropPaths = 256

// Source is where a dataset's target comes from; only local datasets exist so far (name
// and atproto sources are 0045 and 0054).
type Source struct {
	Local bool `drisl:"local"`
	// Path is a writer-local absolute directory that `dataset update` without a target
	// re-adds (docs/cluster.md "Presets").
	Path string `drisl:"path,omitempty"`
	// Ingest configures producer pushes into a collection (D22 "Ingest").
	Ingest *Ingest `drisl:"ingest,omitempty"`
}

// Ingest is a collection's push policy: Hosts are the exact host names (no wildcards)
// receiving members may fetch item URLs from in private mode (and, when set, in public
// mode); Limits bound each URL file.
type Ingest struct {
	Hosts  []string      `drisl:"hosts,omitempty"`
	Limits *IngestLimits `drisl:"limits,omitempty"`
}

// IngestLimits bound URL files of item pushes; zero fields take the defaults
// (collection.DefaultIngestLimits).
type IngestLimits struct {
	// MaxBytes caps one file.
	MaxBytes int64 `drisl:"max_bytes,omitempty"`
	// Attempts bounds the fetches of one file before the push fails.
	Attempts int64 `drisl:"attempts,omitempty"`
	// Timeout bounds one fetch attempt (a duration such as "2h").
	Timeout string `drisl:"timeout,omitempty"`
}

// MaxIngestHosts bounds an ingest host list.
const MaxIngestHosts = 256

// Policy is a dataset's replication policy (docs/cluster.md "Placement"): Replicas copies
// per blob (All: every eligible member), on storage members whose tags match Where (tag →
// allowed values; every listed tag must match), on distinct values of the Spread tag.
type Policy struct {
	Replicas int64               `drisl:"replicas"`
	All      bool                `drisl:"all,omitempty"`
	Where    map[string][]string `drisl:"where,omitempty"`
	Spread   string              `drisl:"spread,omitempty"`
	// History enables version retention (archive preset); nil follows current state.
	History *History `drisl:"history,omitempty"`
}

// History is a retention schedule (docs/cluster.md "Retention"): the KeepLast newest
// versions, then the newest version in each of the first N hour, day, week (7 d), month
// (30 d) and year (365 d) buckets measured back from the newest version (-1: every
// bucket), plus labeled versions and the head's; MaxBytes (0: none) bounds the summed
// newBytes of the live set by dropping the oldest unprotected versions.
type History struct {
	KeepLast int64 `drisl:"keep_last" json:"keep_last"`
	Hourly   int64 `drisl:"hourly" json:"hourly"`
	Daily    int64 `drisl:"daily" json:"daily"`
	Weekly   int64 `drisl:"weekly" json:"weekly"`
	Monthly  int64 `drisl:"monthly" json:"monthly"`
	Yearly   int64 `drisl:"yearly" json:"yearly"`
	MaxBytes int64 `drisl:"max_bytes,omitempty" json:"max_bytes,omitempty"`
}

// ArchiveHistory is the archive preset's schedule (provisional until pilot data, 0034).
func ArchiveHistory() *History {
	return &History{KeepLast: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 12, Yearly: -1}
}

// Validate checks the schedule's ranges.
func (h *History) Validate() error {
	for _, n := range []int64{h.KeepLast, h.Hourly, h.Daily, h.Weekly, h.Monthly, h.Yearly} {
		if n < -1 || n > 1_000_000 {
			return errors.New("history counts must be -1 (all) to 1000000")
		}
	}
	if h.MaxBytes < 0 {
		return errors.New("history max_bytes must not be negative")
	}
	return nil
}

// MaxWhere bounds a policy's tag filter (tags and values per tag).
const MaxWhere = 16

// Validate checks the policy's ranges (a record failing them is ignored).
func (p Policy) Validate() error {
	if p.Replicas < 0 || len(p.Where) > MaxWhere || len(p.Spread) > 64 {
		return errors.New("policy replicas, where or spread out of range")
	}
	for k, vals := range p.Where {
		if k == "" || len(k) > 64 || len(vals) == 0 || len(vals) > MaxWhere {
			return fmt.Errorf("bad where tag %q", k)
		}
	}
	if p.History != nil {
		return p.History.Validate()
	}
	return nil
}

// Head is a dataset's current target as written by one signer.
type Head struct {
	Target string `drisl:"target"`
	Kind   string `drisl:"kind"`
	Size   int64  `drisl:"size,omitempty"`
	Note   string `drisl:"note,omitempty"`
	HLC    string `drisl:"hlc"`
}

func (Head) DRISLType() string { return NSHead }

// Version is a version record `version/<name>.<tid>` (docs/cluster.md "Version tree").
// Its identity is every field except the annotations Label, Note and HLC.
type Version struct {
	Root         string `drisl:"root"`
	Kind         string `drisl:"kind"`
	Parent       string `drisl:"parent,omitempty"`
	Author       string `drisl:"author"`
	Files        int64  `drisl:"files"`
	Bytes        int64  `drisl:"bytes"`
	NewBytes     int64  `drisl:"newBytes"`
	RestoredFrom string `drisl:"restoredFrom,omitempty"`
	Label        string `drisl:"label,omitempty"`
	Note         string `drisl:"note,omitempty"`
	HLC          string `drisl:"hlc"`

	identity string         // hex SHA-256 of the record without annotations
	raw      map[string]any // the decoded record (annotation copies keep unknown fields)
}

func (Version) DRISLType() string { return NSVersion }

// Identity is the hex SHA-256 of the DRISL record without its annotations (set when
// decoded from a record).
func (v *Version) Identity() string { return v.identity }

// Annotated returns a record with v's identity fields (unknown ones included) and the
// given annotations, for a signer annotating a version.
func (v *Version) Annotated(label, note string) RawRecord {
	r := RawRecord{}
	for k, x := range v.raw {
		r[k] = x
	}
	if r["$type"] == nil {
		r["$type"] = NSVersion
	}
	delete(r, "label")
	delete(r, "note")
	if label != "" {
		r["label"] = label
	}
	if note != "" {
		r["note"] = note
	}
	return r
}

// RawRecord is a record written as a generic DRISL map (with its $type).
type RawRecord map[string]any

func (r RawRecord) setHLC(h string) { r["hlc"] = h }

// VersionKey is the record key of a version: <name>.<tid>.
func VersionKey(name, tid string) string { return name + "." + tid }

// SplitVersionKey splits a version record key.
func SplitVersionKey(rkey string) (name, tid string, ok bool) {
	i := strings.LastIndexByte(rkey, '.')
	if i < 0 {
		return "", "", false
	}
	return rkey[:i], rkey[i+1:], true
}

var labelRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// ValidLabel reports whether s may label a version: it matches [a-z][a-z0-9._-]{0,63} and
// does not parse as a TID or a time (so lookups stay unambiguous).
func ValidLabel(s string) bool {
	if !labelRE.MatchString(s) {
		return false
	}
	if _, err := parseTID(s); err == nil {
		return false
	}
	_, err := ParseVersionTime(s)
	return err != nil
}

// Config is cluster-wide configuration.
type Config struct {
	Clock *ClockConfig `drisl:"clock,omitempty"`
	HLC   string       `drisl:"hlc"`
}

func (Config) DRISLType() string { return NSConfig }

// ClockConfig tunes clock handling.
type ClockConfig struct {
	MaxFutureMS int64 `drisl:"max_future_ms,omitempty"`
	WarnSkewMS  int64 `drisl:"warn_skew_ms,omitempty"`
}

// Record is one decoded, schema-valid record of a state repository.
type Record struct {
	Collection, RKey string
	HLC              HLC
	Deleted          bool
	// Value is *Key, *Member, *Invite, *Dataset, *Head, *Version, *Config or *Genesis; nil for
	// tombstones and for history/drop records (whose schemas belong to other tasks).
	Value any
	CID   cid.CID
}

// Path is the record's repository path.
func (r *Record) Path() string { return r.Collection + "/" + r.RKey }

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	inviteRE = regexp.MustCompile(`^[a-z2-7]{16}$`)
)

// ValidName reports whether s is a valid dataset name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

func parseP256(did string) (*atcrypto.PublicKeyP256, error) {
	k, err := atcrypto.ParsePublicDIDKey(did)
	if err != nil {
		return nil, err
	}
	p, ok := k.(*atcrypto.PublicKeyP256)
	if !ok {
		return nil, errors.New("not a P-256 did:key")
	}
	if p.DIDKey() != did {
		return nil, errors.New("did:key is not in canonical form")
	}
	return p, nil
}

var errRecord = errors.New("cluster: invalid record")

func recordErr(path, format string, args ...any) error {
	return fmt.Errorf("%w %s: %s", errRecord, path, fmt.Sprintf(format, args...))
}

// DecodeRecord parses and validates a state-repository record at path (collection/rkey).
// Records that fail are ignored by evaluation, like out-of-scope records.
func DecodeRecord(path string, data []byte) (*Record, error) {
	coll, rkey, ok := strings.Cut(path, "/")
	if !ok {
		return nil, recordErr(path, "path is not collection/rkey")
	}
	if len(data) > MaxRecord {
		return nil, recordErr(path, "%d bytes exceeds %d", len(data), MaxRecord)
	}
	v, err := drisl.DecOptions{MaxSize: MaxRecord, MaxDepth: 8, Atproto: true}.Decode(data)
	if err != nil {
		return nil, recordErr(path, "%v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, recordErr(path, "not a map")
	}
	if t, _ := m["$type"].(string); t != coll {
		return nil, recordErr(path, "$type %q does not match the collection", m["$type"])
	}
	c, err := cid.SumCID(cid.DRISL, cid.SHA256, data)
	if err != nil {
		return nil, err
	}
	r := &Record{Collection: coll, RKey: rkey, CID: c}
	if err := checkRKey(coll, rkey); err != nil {
		return nil, recordErr(path, "%v", err)
	}
	if coll == NSGenesis {
		var g Genesis
		if err := drisl.Assign(v, &g); err != nil {
			return nil, recordErr(path, "%v", err)
		}
		if err := g.Validate(); err != nil {
			return nil, recordErr(path, "%v", err)
		}
		r.Value = &g
		return r, nil
	}
	hs, _ := m["hlc"].(string)
	if r.HLC, err = ParseHLC(hs); err != nil {
		return nil, recordErr(path, "%v", err)
	}
	if del, ok := m["deleted"]; ok {
		if del != true {
			return nil, recordErr(path, "deleted must be true")
		}
		r.Deleted = true
		return r, nil
	}
	if err := decodeValue(r, v); err != nil {
		return nil, recordErr(path, "%v", err)
	}
	return r, nil
}

func checkRKey(coll, rkey string) error {
	switch coll {
	case NSGenesis, NSConfig:
		if rkey != "self" {
			return errors.New("record key must be self")
		}
	case NSKey:
		if _, err := parseP256(rkey); err != nil {
			return fmt.Errorf("record key: %v", err)
		}
	case NSMember:
		id, err := wire.ParsePeerID(rkey)
		if err != nil || id.String() != rkey {
			return errors.New("record key must be a base58 PeerId")
		}
	case NSInvite:
		if !inviteRE.MatchString(rkey) {
			return errors.New("record key must be 16 base32 characters")
		}
	case NSDataset, NSHead, NSHistory, NSDrop:
		if !ValidName(rkey) {
			return errors.New("record key must be a dataset name")
		}
	case NSVersion:
		name, tid, ok := SplitVersionKey(rkey)
		if !ok || !ValidName(name) {
			return errors.New("record key must be <dataset>.<tid>")
		}
		if _, err := parseTID(tid); err != nil {
			return errors.New("record key must end in a TID")
		}
	default:
		return errors.New("unknown collection")
	}
	return nil
}

func oneOf(s string, allowed ...string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}

func validRoles(roles []string) error {
	if len(roles) == 0 || len(roles) > 2 {
		return errors.New("roles must list storage and/or client")
	}
	for _, r := range roles {
		if !oneOf(r, RoleStorage, RoleClient) {
			return fmt.Errorf("unknown role %q", r)
		}
	}
	return nil
}

func validTags(tags map[string]string) error {
	if len(tags) > 32 {
		return errors.New("too many tags")
	}
	for k, v := range tags {
		if len(k) == 0 || len(k) > 64 || len(v) > 256 {
			return errors.New("tag too long")
		}
	}
	return nil
}

func decodeValue(r *Record, v any) error {
	switch r.Collection {
	case NSKey:
		var k Key
		if err := drisl.Assign(v, &k); err != nil {
			return err
		}
		if !oneOf(k.Role, RoleAdmin, RoleWriter) {
			return fmt.Errorf("unknown key role %q", k.Role)
		}
		if k.Revoked != nil {
			if !k.Revoked.After.Defined() {
				return errors.New("revoked.after is required")
			}
			if _, err := ParseRev(k.Revoked.Rev); err != nil {
				return err
			}
		}
		r.Value = &k
	case NSMember:
		var m Member
		if err := drisl.Assign(v, &m); err != nil {
			return err
		}
		if err := validRoles(m.Roles); err != nil {
			return err
		}
		if !oneOf(m.State, StateActive, StateDraining, StateUnavailable, StateRemoved) {
			return fmt.Errorf("unknown member state %q", m.State)
		}
		if m.Capacity < 0 || m.StoreLimit < 0 || len(m.Addrs) > 16 || len(m.Label) > 256 {
			return errors.New("capacity, addrs or label out of range")
		}
		for _, a := range m.Addrs {
			if _, err := wire.ParseMultiaddr(a, 1024); err != nil {
				return fmt.Errorf("addr %q: %v", a, err)
			}
		}
		if err := validTags(m.Tags); err != nil {
			return err
		}
		r.Value = &m
	case NSInvite:
		var in Invite
		if err := drisl.Assign(v, &in); err != nil {
			return err
		}
		if len(in.ProofKey) != 32 {
			return errors.New("proofKey must be 32 bytes")
		}
		if err := validRoles(in.Roles); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339, in.Expires); err != nil {
			return fmt.Errorf("expires: %v", err)
		}
		if in.Capacity < 0 {
			return errors.New("negative capacity")
		}
		if err := validTags(in.Tags); err != nil {
			return err
		}
		r.Value = &in
	case NSDataset:
		var d Dataset
		if err := drisl.Assign(v, &d); err != nil {
			return err
		}
		if !d.Source.Local {
			return errors.New("only local sources are supported")
		}
		if in := d.Source.Ingest; in != nil {
			if d.Collection == nil || len(in.Hosts) > MaxIngestHosts {
				return errors.New("ingest applies to collections, with at most 256 hosts")
			}
			for _, h := range in.Hosts {
				if h == "" || len(h) > 253 || strings.ContainsAny(h, "*/@[] \t") || (strings.Contains(h, ":") && net.ParseIP(h) == nil) {
					return fmt.Errorf("bad ingest host %q (exact host names or IP literals)", h)
				}
			}
			if l := in.Limits; l != nil && (l.MaxBytes < 0 || l.Attempts < 0 || l.Attempts > 100 || len(l.Timeout) > 32) {
				return errors.New("ingest limits out of range")
			}
		}
		if d.Policy.Replicas < 0 || len(d.Writers) > 256 || len(d.Note) > 1024 {
			return errors.New("policy, writers or note out of range")
		}
		if err := d.Policy.Validate(); err != nil {
			return err
		}
		pm, _ := v.(map[string]any)
		pb, err := drisl.Marshal(pm["policy"])
		if err != nil {
			return fmt.Errorf("policy: %v", err)
		}
		sum := sha256.Sum256(pb)
		d.policyHash = hex.EncodeToString(sum[:])
		if c := d.Collection; c != nil {
			if c.Merge != "" && c.Merge != MergeUnion && c.Merge != MergeLWW {
				return fmt.Errorf("unknown merge mode %q", c.Merge)
			}
			if len(c.Facets) > 32 {
				return errors.New("more than 32 facets")
			}
			for name, f := range c.Facets {
				if len(name) == 0 || len(name) > 64 || len(f.Ptr) > 256 || (f.Ptr != "" && f.Ptr[0] != '/') ||
					!oneOf(f.Type, "time", "number", "string", "bbox") {
					return fmt.Errorf("bad facet %q", name)
				}
			}
			if e := c.Expire; e != nil && (c.Facets[e.Facet].Type != "time" || e.After == "") {
				return errors.New("expire must name a time facet and a duration")
			}
		}
		for _, w := range d.Writers {
			if _, err := parseP256(w); err != nil {
				return fmt.Errorf("writer %q: %v", w, err)
			}
		}
		r.Value = &d
	case NSHead:
		var h Head
		if err := drisl.Assign(v, &h); err != nil {
			return err
		}
		if _, err := cid.Parse(h.Target); err != nil {
			return fmt.Errorf("target: %v", err)
		}
		if !oneOf(h.Kind, "tree", "file", "blob") || h.Size < 0 || len(h.Note) > 1024 {
			return errors.New("kind, size or note out of range")
		}
		r.Value = &h
	case NSConfig:
		var c Config
		if err := drisl.Assign(v, &c); err != nil {
			return err
		}
		r.Value = &c
	case NSDrop:
		var d Drop
		if err := drisl.Assign(v, &d); err != nil {
			return err
		}
		if len(d.Paths) > MaxDropPaths {
			return fmt.Errorf("more than %d drop paths", MaxDropPaths)
		}
		for _, p := range d.Paths {
			if !validItemPath(p) {
				return fmt.Errorf("bad drop path %q", p)
			}
		}
		r.Value = &d
	case NSVersion:
		var ver Version
		if err := drisl.Assign(v, &ver); err != nil {
			return err
		}
		if _, err := cid.Parse(ver.Root); err != nil {
			return fmt.Errorf("root: %v", err)
		}
		if !oneOf(ver.Kind, "tree", "file", "blob") || ver.Files < 0 || ver.Bytes < 0 || ver.NewBytes < 0 || len(ver.Note) > 1024 {
			return errors.New("kind, sizes or note out of range")
		}
		if ver.Label != "" && !ValidLabel(ver.Label) {
			return fmt.Errorf("bad label %q", ver.Label)
		}
		if _, err := parseP256(ver.Author); err != nil {
			return fmt.Errorf("author: %v", err)
		}
		for _, t := range []string{ver.Parent, ver.RestoredFrom} {
			if _, err := parseTID(t); t != "" && err != nil {
				return fmt.Errorf("bad version reference %q", t)
			}
		}
		m, _ := v.(map[string]any)
		ver.raw = m
		id := map[string]any{}
		for k, x := range m {
			if k != "label" && k != "note" && k != "hlc" {
				id[k] = x
			}
		}
		b, err := drisl.Marshal(id)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		ver.identity = hex.EncodeToString(sum[:])
		r.Value = &ver
	case NSHistory:
		// The schema belongs to version history (D20); only the envelope ($type, hlc) is
		// checked here.
	}
	return nil
}

// validItemPath reports whether p is a slash-separated path of valid tree entry names.
func validItemPath(p string) bool {
	if p == "" || len(p) > 1024 {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}

// ParseRev parses a commit rev (TID).
func ParseRev(rev string) (uint64, error) {
	if len(rev) != 13 {
		return 0, fmt.Errorf("cluster: rev %q is not a TID", rev)
	}
	t, err := parseTID(rev)
	if err != nil {
		return 0, fmt.Errorf("cluster: rev %q: %w", rev, err)
	}
	return t, nil
}

// encodeRecord marshals a typed record value with its HLC set.
func encodeRecord(v any) ([]byte, error) {
	b, err := drisl.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxRecord {
		return nil, fmt.Errorf("cluster: record of %d bytes exceeds %d", len(b), MaxRecord)
	}
	return b, nil
}

func tombstone(coll string, h HLC) ([]byte, error) {
	return drisl.Marshal(map[string]any{"$type": coll, "deleted": true, "hlc": h.String()})
}
