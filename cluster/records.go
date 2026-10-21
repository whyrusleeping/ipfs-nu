package cluster

import (
	"errors"
	"fmt"
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
	Note    string   `drisl:"note,omitempty"`
	HLC     string   `drisl:"hlc"`
}

func (Dataset) DRISLType() string { return NSDataset }

// Source is where a dataset's target comes from; only local datasets exist so far.
type Source struct {
	Local bool `drisl:"local"`
}

// Policy is a dataset's replication policy (placement is a later task; only replicas is
// recorded).
type Policy struct {
	Replicas int64 `drisl:"replicas"`
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
	// Value is *Key, *Member, *Invite, *Dataset, *Head, *Config or *Genesis; nil for
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
		if d.Policy.Replicas < 0 || len(d.Writers) > 256 || len(d.Note) > 1024 {
			return errors.New("policy, writers or note out of range")
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
	case NSHistory, NSDrop:
		// Schemas belong to version history (D20) and collections (D22); only the
		// envelope ($type, hlc) is checked here.
	}
	return nil
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
