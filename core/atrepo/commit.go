package atrepo

import (
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Version is the repository format version Nu reads and writes.
const Version = 3

// DefaultMaxDrift is how far in the future a commit rev may lie before it is refused.
const DefaultMaxDrift = 5 * time.Minute

var (
	ErrCommit    = errors.New("atrepo: invalid commit")
	ErrSignature = errors.New("atrepo: commit signature does not verify")
	ErrFutureRev = errors.New("atrepo: commit rev lies in the future")
)

// Commit is a v3 repository commit {did, version: 3, data, rev, prev, sig}.
type Commit struct {
	DID  string
	Data cid.CID // MST root
	Rev  string  // TID
	Prev cid.CID // zero for null (Nu always writes null)
	Sig  []byte
}

func (c *Commit) fields(signed bool) map[string]any {
	m := map[string]any{
		"did":     c.DID,
		"version": int64(Version),
		"data":    c.Data,
		"rev":     c.Rev,
		"prev":    optLink(c.Prev),
	}
	if signed {
		m["sig"] = c.Sig
	}
	return m
}

// Unsigned returns the DRISL encoding of the commit without sig: the signed message.
func (c *Commit) Unsigned() ([]byte, error) { return drisl.Marshal(c.fields(false)) }

// Encode returns the signed commit block and its CID.
func (c *Commit) Encode() (cid.CID, []byte, error) {
	if len(c.Sig) == 0 {
		return cid.CID{}, nil, fmt.Errorf("%w: unsigned", ErrCommit)
	}
	return drisl.Sum(c.fields(true))
}

// Sign sets Sig with key (P-256 or secp256k1; low-S, 64-byte compact signature).
func (c *Commit) Sign(key atcrypto.PrivateKey) error {
	msg, err := c.Unsigned()
	if err != nil {
		return err
	}
	c.Sig, err = key.HashAndSign(msg)
	return err
}

// Verify checks Sig against key. atproto requires low-S compact signatures.
func (c *Commit) Verify(key atcrypto.PublicKey) error {
	msg, err := c.Unsigned()
	if err != nil {
		return err
	}
	if err := key.HashAndVerify(msg, c.Sig); err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	return nil
}

// CheckRev refuses revs more than maxDrift after now (0 means DefaultMaxDrift).
func (c *Commit) CheckRev(now time.Time, maxDrift time.Duration) error {
	if maxDrift == 0 {
		maxDrift = DefaultMaxDrift
	}
	t, err := syntax.ParseTID(c.Rev)
	if err != nil {
		return fmt.Errorf("%w: rev: %v", ErrCommit, err)
	}
	if t.Time().After(now.Add(maxDrift)) {
		return fmt.Errorf("%w: rev %s is %s", ErrFutureRev, c.Rev, t.Time().UTC().Format(time.RFC3339))
	}
	return nil
}

// DecodeCommit parses a commit block strictly: exactly the v3 fields, a valid DID and TID
// rev, SHA-256 DRISL links, and a signature of at most 128 bytes.
func DecodeCommit(data []byte) (*Commit, error) {
	v, err := drisl.DecOptions{MaxSize: 64 << 10, MaxDepth: 2, Atproto: true}.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCommit, err)
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 6 {
		return nil, fmt.Errorf("%w: want exactly {did, version, data, rev, prev, sig}", ErrCommit)
	}
	c := &Commit{}
	var ok1, ok2, ok3, ok4 bool
	c.DID, ok1 = m["did"].(string)
	c.Data, ok2 = m["data"].(cid.CID)
	c.Rev, ok3 = m["rev"].(string)
	c.Sig, ok4 = m["sig"].([]byte)
	pv, ok5 := m["prev"]
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil, fmt.Errorf("%w: bad field types", ErrCommit)
	}
	if ver, _ := m["version"].(int64); ver != Version {
		return nil, fmt.Errorf("%w: version %v (only 3 is supported)", ErrCommit, m["version"])
	}
	if _, err := syntax.ParseDID(c.DID); err != nil {
		return nil, fmt.Errorf("%w: did: %v", ErrCommit, err)
	}
	if _, err := syntax.ParseTID(c.Rev); err != nil {
		return nil, fmt.Errorf("%w: rev: %v", ErrCommit, err)
	}
	if c.Data.Codec() != cid.DRISL || c.Data.Hash() != cid.SHA256 {
		return nil, fmt.Errorf("%w: data link is not a SHA-256 DRISL CID", ErrCommit)
	}
	if c.Prev, err = treeLink(pv); err != nil {
		return nil, fmt.Errorf("%w: prev: %v", ErrCommit, err)
	}
	if len(c.Sig) == 0 || len(c.Sig) > 128 {
		return nil, fmt.Errorf("%w: signature length %d", ErrCommit, len(c.Sig))
	}
	return c, nil
}

// ParseKey parses a public signing key given as a did:key or as a DID document
// publicKeyMultibase value (P-256 or secp256k1).
func ParseKey(s string) (atcrypto.PublicKey, error) {
	if len(s) > 8 && s[:8] == "did:key:" {
		return atcrypto.ParsePublicDIDKey(s)
	}
	return atcrypto.ParsePublicMultibase(s)
}

// NextRev returns a TID rev for a commit made at now that sorts after prev ("" for none).
func NextRev(prev string, now time.Time) string {
	r := syntax.NewTIDFromTime(now, 0)
	if p, err := syntax.ParseTID(prev); err == nil && r.Integer() <= p.Integer() {
		r = syntax.NewTIDFromInteger(p.Integer() + 1)
	}
	return r.String()
}

// ValidPath checks a repo path written by Nu: <NSID>/<record key>.
func ValidPath(p string) error {
	_, _, err := syntax.ParseRepoPath(p)
	return err
}
