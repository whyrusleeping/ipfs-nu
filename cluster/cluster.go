package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// StateFile is the per-cluster state file under the cluster directory; KeysDir holds the
// node's authority keys.
const (
	StateFile = "state.json"
	KeysDir   = "keys"
	// MaxPullBlocks bounds one repository diff.
	MaxPullBlocks = 100_000
)

var (
	ErrNoKey     = errors.New("cluster: this node holds no key allowed to write that record")
	ErrNotHeld   = errors.New("cluster: repository not held")
	ErrStale     = errors.New("cluster: commit is not newer than the held one")
	ErrForbidden = errors.New("cluster: repository is not authorized")
)

// Options configure a Cluster.
type Options struct {
	Dir   string // the cluster's directory (state file and keys)
	Store *store.Store
	Now   func() time.Time
	// MaxFuture is clock.max_future (0: DefaultMaxFuture).
	MaxFuture time.Duration
}

type stateFile struct {
	Version int               `json:"version"`
	Genesis Genesis           `json:"genesis"`
	Keys    []string          `json:"keys,omitempty"` // admin keys named by the join token
	Boot    []bootJSON        `json:"boot,omitempty"`
	Heads   map[string]string `json:"heads"`
}

type bootJSON struct {
	Peer  string   `json:"peer"`
	Addrs []string `json:"addrs,omitempty"`
}

// Cluster is one cluster's state on this node: the held state repositories, the node's
// authority keys and the merged view.
type Cluster struct {
	opts  Options
	ID    cid.CID
	clock Clock

	mu    sync.Mutex
	file  stateFile
	keys  map[string]*atcrypto.PrivateKeyP256
	cache map[cid.CID]*RepoRecords
	view  *View
	subs  []func()
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Exists reports whether dir holds a cluster.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, StateFile))
	return err == nil
}

// Create founds a cluster: a new root key and genesis. For a networked cluster the node
// also receives an admin key and self becomes the first storage member (addrs advertised);
// the local cluster's root key writes everything.
func Create(opts Options, name, mode string, self wire.PeerID, addrs []string) (*Cluster, error) {
	if Exists(opts.Dir) {
		return nil, fmt.Errorf("cluster: %s already holds a cluster", opts.Dir)
	}
	rootKey, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	rootPub, _ := rootKey.PublicKey()
	g := Genesis{Name: name, Mode: mode, RootKey: rootPub.DIDKey(), CreatedAt: FormatTime(opts.now())}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	c, err := newCluster(opts, stateFile{Version: 1, Genesis: g, Heads: map[string]string{}})
	if err != nil {
		return nil, err
	}
	if err := c.saveKey(rootKey); err != nil {
		return nil, err
	}
	ops := []Op{{Collection: NSGenesis, RKey: "self", Value: &g}}
	if mode != ModeLocal {
		adminKey, err := atcrypto.GeneratePrivateKeyP256()
		if err != nil {
			return nil, err
		}
		if err := c.saveKey(adminKey); err != nil {
			return nil, err
		}
		apub, _ := adminKey.PublicKey()
		ops = append(ops,
			Op{Collection: NSKey, RKey: apub.DIDKey(), Value: &Key{Role: RoleAdmin}},
			Op{Collection: NSMember, RKey: self.String(), Value: &Member{Roles: []string{RoleStorage}, Addrs: addrs, State: StateActive}})
	}
	if _, err := c.writeAs(g.RootKey, ops); err != nil {
		return nil, err
	}
	return c, nil
}

// JoinToken prepares a cluster from an invite token: genesis, admin keys and boot peers.
// Nothing is synced yet.
func JoinToken(opts Options, t *Token) (*Cluster, error) {
	if Exists(opts.Dir) {
		return nil, fmt.Errorf("cluster: %s already holds a cluster", opts.Dir)
	}
	f := stateFile{Version: 1, Genesis: t.Genesis, Keys: t.Keys, Heads: map[string]string{}}
	for _, b := range t.Boot {
		f.Boot = append(f.Boot, bootJSON{Peer: b.Peer.String(), Addrs: b.Addrs})
	}
	c, err := newCluster(opts, f)
	if err != nil {
		return nil, err
	}
	return c, c.saveLocked()
}

// Open loads a cluster from its directory.
func Open(opts Options) (*Cluster, error) {
	b, err := os.ReadFile(filepath.Join(opts.Dir, StateFile))
	if err != nil {
		return nil, err
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("cluster: %s: %w", StateFile, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("cluster: %s version %d is not supported", StateFile, f.Version)
	}
	if f.Heads == nil {
		f.Heads = map[string]string{}
	}
	c, err := newCluster(opts, f)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(opts.Dir, KeysDir))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(opts.Dir, KeysDir, e.Name()))
		if err != nil {
			return nil, err
		}
		k, err := atcrypto.ParsePrivateBytesP256(raw)
		if err != nil {
			return nil, fmt.Errorf("cluster: key %s: %w", e.Name(), err)
		}
		pub, _ := k.PublicKey()
		c.keys[pub.DIDKey()] = k
	}
	c.mu.Lock()
	c.evaluateLocked()
	c.mu.Unlock()
	return c, nil
}

func newCluster(opts Options, f stateFile) (*Cluster, error) {
	if err := f.Genesis.Validate(); err != nil {
		return nil, err
	}
	id, err := f.Genesis.ID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, KeysDir), 0o700); err != nil {
		return nil, err
	}
	opts.Store.RegisterProfile("atrepo", atrepo.Links)
	c := &Cluster{opts: opts, ID: id, file: f, keys: map[string]*atcrypto.PrivateKeyP256{}, cache: map[cid.CID]*RepoRecords{}}
	c.clock.Now = opts.now
	c.evaluateLocked()
	return c, nil
}

func (c *Cluster) saveKey(k *atcrypto.PrivateKeyP256) error {
	pub, _ := k.PublicKey()
	name := strings.TrimPrefix(pub.DIDKey(), "did:key:") + ".key"
	if err := writeAtomic(filepath.Join(c.opts.Dir, KeysDir, name), k.Bytes()); err != nil {
		return err
	}
	c.mu.Lock()
	c.keys[pub.DIDKey()] = k
	c.mu.Unlock()
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (c *Cluster) saveLocked() error {
	b, err := json.MarshalIndent(c.file, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(c.opts.Dir, StateFile), b)
}

// Genesis returns the cluster's genesis record.
func (c *Cluster) Genesis() Genesis { return c.file.Genesis }

// Mode is the network mode fixed at genesis.
func (c *Cluster) Mode() string { return c.file.Genesis.Mode }

// Boot returns the bootstrap peers from the join token.
func (c *Cluster) Boot() []Boot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Boot
	for _, b := range c.file.Boot {
		if id, err := wire.ParsePeerID(b.Peer); err == nil {
			out = append(out, Boot{Peer: id, Addrs: b.Addrs})
		}
	}
	return out
}

// View returns the current merged state. Callers must not modify it.
func (c *Cluster) View() *View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// Refresh re-evaluates (held-back records become visible as time passes).
func (c *Cluster) Refresh() {
	c.mu.Lock()
	c.evaluateLocked()
	c.mu.Unlock()
}

// OnChange registers fn, called (without locks held) after every change of held state.
func (c *Cluster) OnChange(fn func()) {
	c.mu.Lock()
	c.subs = append(c.subs, fn)
	c.mu.Unlock()
}

func (c *Cluster) notify() {
	c.mu.Lock()
	subs := slices.Clone(c.subs)
	c.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

// HeldKeys lists the DIDs of the authority keys this node holds.
func (c *Cluster) HeldKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.keys))
	for did := range c.keys {
		out = append(out, did)
	}
	slices.Sort(out)
	return out
}

// HeadInfo is one held repository head.
type HeadInfo struct {
	DID    string
	Rev    string
	Commit cid.CID
}

// Heads lists the held repository heads.
func (c *Cluster) Heads() []HeadInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []HeadInfo
	for did, s := range c.file.Heads {
		h, err := cid.Parse(s)
		if err != nil {
			continue
		}
		rr := c.recordsLocked(h)
		if rr == nil {
			continue
		}
		out = append(out, HeadInfo{DID: did, Rev: rr.Rev, Commit: h})
	}
	slices.SortFunc(out, func(a, b HeadInfo) int { return strings.Compare(a.DID, b.DID) })
	return out
}

// Held returns the held head of did.
func (c *Cluster) Held(did string) (HeadInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heldLocked(did)
}

func (c *Cluster) heldLocked(did string) (HeadInfo, bool) {
	s, ok := c.file.Heads[did]
	if !ok {
		return HeadInfo{}, false
	}
	h, err := cid.Parse(s)
	if err != nil {
		return HeadInfo{}, false
	}
	rr := c.recordsLocked(h)
	if rr == nil {
		return HeadInfo{}, false
	}
	return HeadInfo{DID: did, Rev: rr.Rev, Commit: h}, true
}

// recordsLocked decodes the records of commit h (cached); nil if not fully held.
func (c *Cluster) recordsLocked(h cid.CID) *RepoRecords {
	if rr, ok := c.cache[h]; ok {
		return rr
	}
	r, err := atrepo.Open(c.opts.Store, h)
	if err != nil {
		return nil
	}
	rr := &RepoRecords{Commit: h, Rev: r.Head.Rev}
	err = r.Tree.Walk("", func(path string, rc cid.CID) (bool, error) {
		data, err := c.opts.Store.Get(rc)
		if err != nil {
			return false, err
		}
		rec, err := DecodeRecord(path, data)
		if err != nil {
			rr.Invalid++
			return true, nil
		}
		rr.Records = append(rr.Records, rec)
		return true, nil
	})
	if err != nil {
		return nil
	}
	if len(c.cache) > 4096 {
		clear(c.cache)
	}
	c.cache[h] = rr
	return rr
}

// source adapts the held repositories for Evaluate (called with c.mu held).
type source struct{ c *Cluster }

func (s source) DIDs() []string {
	out := make([]string, 0, len(s.c.file.Heads))
	for did := range s.c.file.Heads {
		out = append(out, did)
	}
	return out
}

func (s source) Head(did string) *RepoRecords {
	h, ok := s.c.file.Heads[did]
	if !ok {
		return nil
	}
	hc, err := cid.Parse(h)
	if err != nil {
		return nil
	}
	return s.c.recordsLocked(hc)
}

func (s source) At(did string, at cid.CID) *RepoRecords {
	rr := s.c.recordsLocked(at)
	if rr == nil {
		return nil
	}
	r, err := atrepo.Open(s.c.opts.Store, at)
	if err != nil || r.DID != did {
		return nil
	}
	return rr
}

func (c *Cluster) evaluateLocked() {
	c.view = Evaluate(c.file.Genesis, source{c}, c.opts.now(), c.opts.MaxFuture)
	c.clock.Observe(c.view.MaxHLC)
}

// Op is one record write: a value (a record type from this package) or a deletion
// (tombstone).
type Op struct {
	Collection, RKey string
	Value            hlcRecord
	Delete           bool
}

type hlcRecord interface{ setHLC(string) }

func (g *Genesis) setHLC(string)   {}
func (k *Key) setHLC(h string)     { k.HLC = h }
func (m *Member) setHLC(h string)  { m.HLC = h }
func (i *Invite) setHLC(h string)  { i.HLC = h }
func (d *Dataset) setHLC(h string) { d.HLC = h }
func (h *Head) setHLC(s string)    { h.HLC = s }
func (c *Config) setHLC(h string)  { c.HLC = h }

// Written is the outcome of a local write.
type Written struct {
	DID    string
	Commit cid.CID
	Rev    string
	HLC    HLC
}

// mayWrite reports whether a key with role may write coll/rkey under view v.
func mayWrite(v *View, did, role, coll, rkey string) bool {
	switch role {
	case RoleRoot:
		return coll != NSGenesis
	case RoleAdmin:
		return coll != NSGenesis && !(coll == NSKey && rkey == v.Root)
	case RoleWriter:
		if coll != NSHead && coll != NSHistory {
			return false
		}
		d, ok := v.Dataset(rkey)
		return ok && slices.Contains(d.Writers, did)
	}
	return false
}

// Write commits ops to the repository of a held key allowed to write all of them,
// preferring an admin key over the root key.
func (c *Cluster) Write(ops ...Op) (*Written, error) {
	c.mu.Lock()
	v := c.view
	var cands []string
	for did := range c.keys {
		if k := v.Keys[did]; k != nil && k.Revoked == nil {
			cands = append(cands, did)
		}
	}
	c.mu.Unlock()
	rank := map[string]int{RoleAdmin: 0, RoleRoot: 1, RoleWriter: 2}
	slices.SortFunc(cands, func(a, b string) int {
		if d := rank[v.Keys[a].Role] - rank[v.Keys[b].Role]; d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	for _, did := range cands {
		ok := true
		for _, op := range ops {
			if !mayWrite(v, did, v.Keys[did].Role, op.Collection, op.RKey) {
				ok = false
				break
			}
		}
		if ok {
			return c.writeAs(did, ops)
		}
	}
	return nil, ErrNoKey
}

// WriteAs commits ops signed by a specific held key without checking authorization
// (tests and tools that need to produce out-of-scope records).
func (c *Cluster) WriteAs(did string, ops ...Op) (*Written, error) { return c.writeAs(did, ops) }

func (c *Cluster) writeAs(did string, ops []Op) (*Written, error) {
	c.mu.Lock()
	key := c.keys[did]
	if key == nil {
		c.mu.Unlock()
		return nil, ErrNoKey
	}
	st := c.opts.Store
	var r *atrepo.Repo
	var old cid.CID
	if h, ok := c.heldLocked(did); ok {
		var err error
		if r, err = atrepo.Open(st, h.Commit); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		old = h.Commit
	} else {
		r = atrepo.New(st, did)
	}
	hlc := c.clock.Next()
	for _, op := range ops {
		path := op.Collection + "/" + op.RKey
		var b []byte
		var err error
		if op.Delete {
			b, err = tombstone(op.Collection, hlc)
		} else {
			op.Value.setHLC(hlc.String())
			b, err = encodeRecord(op.Value)
		}
		if err == nil {
			_, err = r.Put(path, b)
		}
		if err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("cluster: write %s: %w", path, err)
		}
	}
	commit, err := r.Commit(key, c.opts.now())
	if err == nil {
		err = c.adoptLocked(did, commit, old)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c.notify()
	return &Written{DID: did, Commit: commit, Rev: r.Head.Rev, HLC: hlc}, nil
}

// adoptLocked makes commit the held head of did: pin, persist, re-evaluate.
func (c *Cluster) adoptLocked(did string, commit, old cid.CID) error {
	pin := store.Pin{CID: commit, Mode: store.Recursive, Profile: "atrepo", Source: store.SourceCluster, Name: "state/" + c.ID.String() + "/" + did}
	if err := c.opts.Store.Pin(pin); err != nil {
		return err
	}
	c.file.Heads[did] = commit.String()
	if err := c.saveLocked(); err != nil {
		return err
	}
	if old.Defined() && old != commit {
		pin.CID = old
		_, _ = c.opts.Store.Unpin(pin)
	}
	c.evaluateLocked()
	return nil
}

// Wants returns the DIDs this node should pull, given a peer's heads: authorized (or
// token-named) repositories with a newer rev, and revocation cutoffs not yet held. The
// map value is the specific commit to ask for (zero: the head).
func (c *Cluster) Wants(peer []HeadInfo) map[string]cid.CID {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.view
	out := map[string]cid.CID{}
	for _, h := range peer {
		k := v.Keys[h.DID]
		if k == nil && !slices.Contains(c.file.Keys, h.DID) {
			continue
		}
		if k != nil && k.Revoked != nil {
			if rs := v.Repos[h.DID]; rs != nil && rs.Want.Defined() {
				out[h.DID] = rs.Want
			}
			continue
		}
		held, ok := c.heldLocked(h.DID)
		if !ok || revLess(held.Rev, h.Rev) {
			out[h.DID] = cid.CID{}
		}
	}
	return out
}

func revLess(a, b string) bool {
	x, err1 := ParseRev(a)
	y, err2 := ParseRev(b)
	return err1 == nil && err2 == nil && x < y
}

// SinceFor returns the data root to send as `since` when pulling did (zero if none held).
func (c *Cluster) SinceFor(did string) cid.CID {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.heldLocked(did)
	if !ok {
		return cid.CID{}
	}
	r, err := atrepo.Open(c.opts.Store, h.Commit)
	if err != nil {
		return cid.CID{}
	}
	return r.Head.Data
}

// Apply verifies and adopts a repository diff CAR for did (a `pull` response): the
// signature against the did:key, the diff against the held tree, and the rev order. want,
// if defined, is the revocation cutoff being fetched. It reports whether the held state
// changed.
func (c *Cluster) Apply(did string, want cid.CID, r io.Reader) (bool, error) {
	c.mu.Lock()
	v := c.view
	k := v.Keys[did]
	if k == nil && !slices.Contains(c.file.Keys, did) {
		c.mu.Unlock()
		return false, ErrForbidden
	}
	pub, err := parseP256(did)
	if err != nil {
		c.mu.Unlock()
		return false, err
	}
	held, haveHeld := c.heldLocked(did)
	var prevData cid.CID
	if haveHeld {
		if hr, err := atrepo.Open(c.opts.Store, held.Commit); err == nil {
			prevData = hr.Head.Data
		}
	}
	c.mu.Unlock()

	maxFuture := c.opts.MaxFuture
	if maxFuture <= 0 {
		maxFuture = DefaultMaxFuture
	}
	a, err := atrepo.ApplySince(c.opts.Store, prevData, r, atrepo.VerifyOptions{DID: did, Key: pub, Now: c.opts.now(), MaxDrift: maxFuture, MaxBlocks: MaxPullBlocks})
	if err != nil {
		return false, err
	}
	switch {
	case k != nil && k.Revoked != nil:
		if a.CommitCID != k.Revoked.After {
			return false, fmt.Errorf("%w: %s is revoked after %s", ErrStale, did, k.Revoked.After)
		}
	case want.Defined() && a.CommitCID != want:
		return false, fmt.Errorf("cluster: pulled %s, want %s", a.CommitCID, want)
	case haveHeld && !revLess(held.Rev, a.Commit.Rev):
		return false, ErrStale
	}
	for _, b := range a.Blocks {
		if err := c.opts.Store.Put(b.CID, b.Data); err != nil {
			return false, err
		}
	}
	c.mu.Lock()
	cur, _ := c.heldLocked(did)
	if cur.Commit != held.Commit {
		c.mu.Unlock()
		return false, ErrStale // raced with another apply
	}
	err = c.adoptLocked(did, a.CommitCID, held.Commit)
	c.mu.Unlock()
	if err != nil {
		return false, err
	}
	c.notify()
	return true, nil
}

// WriteDiff writes the CAR answering a pull: the repo diff from the since tree to the held
// head of did (or to commit at, when defined), or a full export when the since tree is
// not held.
func (c *Cluster) WriteDiff(w io.Writer, did string, since, at cid.CID) error {
	target := at
	if !target.Defined() {
		h, ok := c.Held(did)
		if !ok {
			return ErrNotHeld
		}
		target = h.Commit
	}
	r, err := atrepo.Open(c.opts.Store, target)
	if err != nil || r.DID != did {
		return ErrNotHeld
	}
	if since.Defined() {
		if blocks, _, err := atrepo.Slice(c.opts.Store, since, target); err == nil {
			return atrepo.WriteCAR(w, target, blocks)
		}
	}
	return atrepo.Export(w, c.opts.Store, target)
}

// HoldsKey reports whether this node holds a non-revoked key of role (or root when role
// is admin).
func (c *Cluster) HoldsKey(role string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for did := range c.keys {
		if k := c.view.Keys[did]; k != nil && k.Revoked == nil && (k.Role == role || (role == RoleAdmin && k.Role == RoleRoot)) {
			return true
		}
	}
	return false
}

// Now is the cluster's clock.
func (c *Cluster) Now() time.Time { return c.opts.now() }

// NewKey generates and stores a new P-256 authority key on this node and returns its
// did:key; an admin then grants it a role with a key record.
func (c *Cluster) NewKey() (string, error) {
	k, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return "", err
	}
	if err := c.saveKey(k); err != nil {
		return "", err
	}
	pub, _ := k.PublicKey()
	return pub.DIDKey(), nil
}
