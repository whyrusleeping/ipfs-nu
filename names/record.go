package names

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Collections of Nu's atproto records (docs/names.md D5). The namespace io.ipfs.nu is
// owned by nu.ipfs.io.
const (
	CollectionName = "io.ipfs.nu.name"
	CollectionNode = "io.ipfs.nu.node"
)

// Kinds of a name's target.
const (
	KindTree = "tree"
	KindFile = "file"
	KindBlob = "blob"
)

// Roles of a node record.
const (
	RoleDevice = "device"
	RoleSeed   = "seed"
)

// Name is an `io.ipfs.nu.name/<name>` record: a public name's current target. Target is
// a string so the PDS never has to accept BLAKE3 CIDs as repository links.
type Name struct {
	Target    string `json:"target" drisl:"target"`
	Kind      string `json:"kind" drisl:"kind"`
	Size      int64  `json:"size,omitempty" drisl:"size,omitempty"`
	Note      string `json:"note,omitempty" drisl:"note,omitempty"`
	UpdatedAt string `json:"updatedAt" drisl:"updatedAt"`
}

// Node is an `io.ipfs.nu.node/<peerId>` record binding a DID to a node: its roles, relay
// circuits and dnsaddr name, never raw IP addresses.
type Node struct {
	Label     string   `json:"label,omitempty" drisl:"label,omitempty"`
	Roles     []string `json:"roles" drisl:"roles"`
	Relays    []string `json:"relays,omitempty" drisl:"relays,omitempty"`
	DNSAddr   string   `json:"dnsaddr,omitempty" drisl:"dnsaddr,omitempty"`
	CreatedAt string   `json:"createdAt" drisl:"createdAt"`
}

// Bounds of a record.
const (
	MaxNameLen  = 64
	MaxNote     = 1024
	MaxLabel    = 64
	MaxRelays   = 8
	MaxRelayLen = 512
)

// nameRE is the rkey grammar of a name: flat, lowercase-insensitive URL-safe tokens.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName checks a public name (the record key): 1–64 characters of [A-Za-z0-9._-],
// not starting with a dot or dash and never "." or "..".
func ValidName(name string) error {
	if !nameRE.MatchString(name) || name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("name %q must be 1-%d characters of letters, digits, '.', '_' or '-', starting with a letter or digit", name, MaxNameLen)
	}
	return nil
}

// Validate checks a name record's fields and returns its target CID.
func (n *Name) Validate() (cid.CID, error) {
	c, err := cid.Parse(n.Target)
	if err != nil {
		return cid.CID{}, fmt.Errorf("target %q is not a DASL CID: %v", n.Target, err)
	}
	switch n.Kind {
	case KindTree, KindFile:
		if c.Codec() != cid.DRISL {
			return cid.CID{}, fmt.Errorf("a %s target must be a DRISL CID, not %s", n.Kind, n.Target)
		}
	case KindBlob:
		if c.Codec() != cid.Raw {
			return cid.CID{}, fmt.Errorf("a blob target must be a raw CID, not %s", n.Target)
		}
	default:
		return cid.CID{}, fmt.Errorf("kind must be tree, file or blob, not %q", n.Kind)
	}
	if n.Size < 0 {
		return cid.CID{}, errors.New("size must not be negative")
	}
	if len(n.Note) > MaxNote {
		return cid.CID{}, fmt.Errorf("note exceeds %d bytes", MaxNote)
	}
	if _, err := time.Parse(time.RFC3339, n.UpdatedAt); err != nil {
		return cid.CID{}, fmt.Errorf("updatedAt %q is not an RFC 3339 time", n.UpdatedAt)
	}
	return c, nil
}

// Validate checks a node record.
func (n *Node) Validate() error {
	if len(n.Label) > MaxLabel {
		return fmt.Errorf("label exceeds %d bytes", MaxLabel)
	}
	if len(n.Roles) == 0 {
		return errors.New("roles must name device or seed")
	}
	for _, r := range n.Roles {
		if r != RoleDevice && r != RoleSeed {
			return fmt.Errorf("unknown role %q (device, seed)", r)
		}
	}
	if len(n.Relays) > MaxRelays {
		return fmt.Errorf("at most %d relays", MaxRelays)
	}
	for _, r := range n.Relays {
		if len(r) > MaxRelayLen || !strings.HasPrefix(r, "/dns") || !strings.HasSuffix(r, "/p2p-circuit") {
			return fmt.Errorf("relay %q must be a /dns… circuit address ending in /p2p-circuit", r)
		}
	}
	if n.DNSAddr != "" && !strings.HasPrefix(n.DNSAddr, "/dnsaddr/") {
		return fmt.Errorf("dnsaddr %q must start with /dnsaddr/", n.DNSAddr)
	}
	if _, err := time.Parse(time.RFC3339, n.CreatedAt); err != nil {
		return fmt.Errorf("createdAt %q is not an RFC 3339 time", n.CreatedAt)
	}
	return nil
}

// HasRole reports whether the node record carries role r.
func (n *Node) HasRole(r string) bool {
	for _, x := range n.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// DecodeName decodes a verified record's DRISL bytes as a name record.
func DecodeName(data []byte) (*Name, error) {
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	if m == nil || m["$type"] != CollectionName {
		return nil, fmt.Errorf("record is not an %s record", CollectionName)
	}
	var n Name
	if err := drisl.Assign(m, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// DecodeNode decodes a verified record's DRISL bytes as a node record.
func DecodeNode(data []byte) (*Node, error) {
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	if m == nil || m["$type"] != CollectionNode {
		return nil, fmt.Errorf("record is not an %s record", CollectionNode)
	}
	var n Node
	if err := drisl.Assign(m, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// Record returns the JSON-ready record map with $type, as written to a PDS.
func (n *Name) Record() map[string]any {
	m := map[string]any{"$type": CollectionName, "target": n.Target, "kind": n.Kind, "updatedAt": n.UpdatedAt}
	if n.Size > 0 {
		m["size"] = n.Size
	}
	if n.Note != "" {
		m["note"] = n.Note
	}
	return m
}

// Record returns the JSON-ready record map with $type.
func (n *Node) Record() map[string]any {
	m := map[string]any{"$type": CollectionNode, "roles": n.Roles, "createdAt": n.CreatedAt}
	if n.Label != "" {
		m["label"] = n.Label
	}
	if len(n.Relays) > 0 {
		m["relays"] = n.Relays
	}
	if n.DNSAddr != "" {
		m["dnsaddr"] = n.DNSAddr
	}
	return m
}
