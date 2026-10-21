package atrepo

import (
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// Store holds repository blocks: commits, MST nodes and records.
type Store = mst.Store

// Repo is a repository at one commit, or a new repository being built.
type Repo struct {
	st      Store
	DID     string
	Head    *Commit // nil before the first commit
	HeadCID cid.CID
	Tree    *mst.Tree[cid.CID]
}

// New starts an empty repository for did.
func New(st Store, did string) *Repo {
	return &Repo{st: st, DID: did, Tree: LoadTree(st, cid.CID{})}
}

// Open opens the repository at commit c. It decodes the commit but does not verify the
// signature or the tree; see Verify.
func Open(st Store, c cid.CID) (*Repo, error) {
	data, err := st.Get(c)
	if err != nil {
		return nil, err
	}
	if err := c.Verify(data); err != nil {
		return nil, err
	}
	com, err := DecodeCommit(data)
	if err != nil {
		return nil, err
	}
	return &Repo{st: st, DID: com.DID, Head: com, HeadCID: c, Tree: LoadTree(st, com.Data)}, nil
}

// Get returns the record at path: its CID and block. ok is false if there is no such
// record; a record the store lacks is an error wrapping mst.ErrNotFound.
func (r *Repo) Get(path string) (c cid.CID, data []byte, ok bool, err error) {
	c, ok, err = r.Tree.Get(path)
	if err != nil || !ok {
		return cid.CID{}, nil, ok, err
	}
	data, err = r.st.Get(c)
	return c, data, err == nil, err
}

// ValidateRecord checks a record block against the atproto data model: at most
// MaxRecordSize bytes of strict DRISL, a map, no floats; foreign CID links are kept.
func ValidateRecord(data []byte) error {
	if len(data) > MaxRecordSize {
		return fmt.Errorf("atrepo: record of %d bytes exceeds %d", len(data), MaxRecordSize)
	}
	v, err := drisl.AtprotoRecord.Decode(data)
	if err != nil {
		return fmt.Errorf("atrepo: record: %w", err)
	}
	return drisl.ValidateAtproto(v)
}

// checkRecord separates hard errors (oversized, not a DRISL map) from atproto data-model
// violations (dm), which imported records may carry: they are kept and reported.
func checkRecord(data []byte) (dm, err error) {
	if len(data) > MaxRecordSize {
		return nil, fmt.Errorf("atrepo: record of %d bytes exceeds %d", len(data), MaxRecordSize)
	}
	v, err := drisl.DecOptions{ForeignLinks: true}.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("atrepo: record: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("atrepo: record is not a map")
	}
	return drisl.ValidateAtproto(v), nil
}

// Put stores record (DRISL bytes) at path, which must be <NSID>/<record key>.
func (r *Repo) Put(path string, record []byte) (cid.CID, error) {
	if err := ValidPath(path); err != nil {
		return cid.CID{}, err
	}
	if err := ValidateRecord(record); err != nil {
		return cid.CID{}, err
	}
	c, err := cid.SumCID(cid.DRISL, cid.SHA256, record)
	if err != nil {
		return cid.CID{}, err
	}
	if err := r.st.Put(c, record); err != nil {
		return cid.CID{}, err
	}
	return c, r.Tree.Put(path, c)
}

// Delete removes the record at path and reports whether it existed.
func (r *Repo) Delete(path string) (bool, error) { return r.Tree.Delete(path) }

// Commit flushes the tree and writes a new commit signed with key, with a rev after the
// previous one. It returns the commit CID.
func (r *Repo) Commit(key atcrypto.PrivateKey, now time.Time) (cid.CID, error) {
	data, err := r.Tree.Flush()
	if err != nil {
		return cid.CID{}, err
	}
	prev := ""
	if r.Head != nil {
		prev = r.Head.Rev
	}
	com := &Commit{DID: r.DID, Data: data, Rev: NextRev(prev, now)}
	if err := com.Sign(key); err != nil {
		return cid.CID{}, err
	}
	c, b, err := com.Encode()
	if err != nil {
		return cid.CID{}, err
	}
	if err := r.st.Put(c, b); err != nil {
		return cid.CID{}, err
	}
	r.Head, r.HeadCID = com, c
	return c, nil
}

// VerifyOptions configures verification of untrusted repository data.
type VerifyOptions struct {
	DID string             // expected repository DID; "" accepts any
	Key atcrypto.PublicKey // signing key; nil skips the signature check (offline import)
	// Now is the clock for the rev drift check; the zero value uses time.Now.
	Now      time.Time
	MaxDrift time.Duration // 0 means DefaultMaxDrift
	// MaxBlocks bounds the blocks read from one CAR; 0 means DefaultMaxBlocks.
	MaxBlocks int
}

func (o VerifyOptions) commit(com *Commit) error {
	if o.DID != "" && com.DID != o.DID {
		return fmt.Errorf("%w: repository DID %s, want %s", ErrCommit, com.DID, o.DID)
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	if err := com.CheckRev(now, o.MaxDrift); err != nil {
		return err
	}
	if o.Key != nil {
		return com.Verify(o.Key)
	}
	return nil
}

// openVerified opens commit c from st and checks it under o.
func openVerified(st Store, c cid.CID, o VerifyOptions) (*Repo, error) {
	r, err := Open(st, c)
	if err != nil {
		return nil, err
	}
	if err := o.commit(r.Head); err != nil {
		return nil, err
	}
	return r, nil
}

// Stats summarises a verified repository.
type Stats struct {
	Records, Nodes int
	// NonConforming lists records that are valid DRISL maps but break the atproto data
	// model (for example floats). They are kept byte-exactly; they do not fail verification.
	NonConforming []string
}

// Verify checks the repository at commit c completely: the commit under o, every MST node
// (structure, depth, order and limits, rebuilt from its blocks so the root equals data) and
// every record (DRISL map, size limit; data-model violations are reported in Stats). Every
// block must be in st. A tree that passes is the canonical tree of its key set, so this
// equals rebuilding the MST and comparing the root with data.
func Verify(st Store, c cid.CID, o VerifyOptions) (*Repo, Stats, error) {
	r, err := openVerified(st, c, o)
	if err != nil {
		return nil, Stats{}, err
	}
	var s Stats
	counting := &recorder{Store: st}
	tr := LoadTree(counting, r.Head.Data)
	err = tr.Walk("", func(k string, v cid.CID) (bool, error) {
		data, err := st.Get(v)
		if err != nil {
			return false, fmt.Errorf("atrepo: record %s (%s): %w", k, v, err)
		}
		if err := v.Verify(data); err != nil {
			return false, err
		}
		dm, err := checkRecord(data)
		if err != nil {
			return false, fmt.Errorf("%s: %w", k, err)
		}
		if dm != nil {
			s.NonConforming = append(s.NonConforming, k)
		}
		s.Records++
		return true, nil
	})
	if err != nil {
		return nil, s, err
	}
	s.Nodes = len(counting.got)
	return r, s, nil
}

// recorder records the CIDs it serves, in order.
type recorder struct {
	Store
	got  []cid.CID
	seen map[cid.CID]bool
}

func (r *recorder) Get(c cid.CID) ([]byte, error) {
	b, err := r.Store.Get(c)
	if err == nil {
		if r.seen == nil {
			r.seen = map[cid.CID]bool{}
		}
		if !r.seen[c] {
			r.seen[c] = true
			r.got = append(r.got, c)
		}
	}
	return b, err
}

// overlay reads from top, then base, and writes to top.
type overlay struct {
	top  mst.MemStore
	base Store
}

func (o *overlay) Get(c cid.CID) ([]byte, error) {
	if b, ok := o.top[c]; ok {
		return b, nil
	}
	if o.base == nil {
		return nil, fmt.Errorf("%w: %s", mst.ErrNotFound, c)
	}
	return o.base.Get(c)
}

func (o *overlay) Put(c cid.CID, data []byte) error {
	o.top[c] = data
	return nil
}

func get(st Store, c cid.CID) (car.Block, error) {
	b, err := st.Get(c)
	return car.Block{CID: c, Data: b}, err
}
