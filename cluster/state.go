package cluster

import (
	"maps"
	"slices"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// DefaultMaxFuture is clock.max_future: records further ahead are held back.
const DefaultMaxFuture = 5 * time.Minute

// maxRounds bounds the key-authorization fixpoint.
const maxRounds = 16

// RepoRecords are the decoded records of one repository at one commit.
type RepoRecords struct {
	Commit  cid.CID
	Rev     string
	Records []*Record
	Invalid int // records that failed their schema
}

// Repos supplies repositories to Evaluate: the held head of a DID, and the records of a
// specific commit (for revocation cutoffs) when held.
type Repos interface {
	// DIDs lists the repositories held.
	DIDs() []string
	// Head returns the held head's records, or nil.
	Head(did string) *RepoRecords
	// At returns the records of commit c of did if held, or nil.
	At(did string, c cid.CID) *RepoRecords
}

// Signed is a winning record with its signer.
type Signed[T any] struct {
	Value   *T
	Signer  string
	HLC     HLC
	Deleted bool
}

// KeyState is an authorized key.
type KeyState struct {
	DID     string
	Role    string // root, admin or writer
	Revoked *Revocation
	Signer  string // "" for the root
	HLC     HLC
}

// RepoState says how a repository was evaluated.
type RepoState struct {
	DID    string
	Held   cid.CID // held head
	Rev    string
	Used   cid.CID // commit evaluated (differs from Held after a revocation cutoff)
	Want   cid.CID // a revocation cutoff not yet held
	Role   string  // "" if the key is not authorized
	Status string  // "ok", "revoked", "waiting" (cutoff not held), "unauthorized"
}

// View is the merged cluster state.
type View struct {
	Genesis Genesis
	ID      cid.CID
	Root    string

	Keys     map[string]*KeyState
	Members  map[wire.PeerID]Signed[Member]
	Invites  map[string]Signed[Invite]
	Datasets map[string]Signed[Dataset]
	Config   Signed[Config]
	// Heads holds every authorized signer's head record per dataset (union-head hook);
	// Head picks the LWW winner.
	Heads map[string]map[string]Signed[Head]
	// History and Drop hold the LWW winners' record CIDs (history's schema is another
	// task); Drops holds the decoded drop records.
	History, Drop map[string]Signed[cid.CID]
	Drops         map[string]Signed[Drop]

	Repos map[string]*RepoState
	// Ignored counts out-of-scope or invalid records; HeldBack counts records whose hlc is
	// too far ahead.
	Ignored, HeldBack int
	// MaxHLC is the greatest hlc seen in merged state.
	MaxHLC HLC
}

type cand struct {
	r      *Record
	signer string
	root   bool
}

// beats reports whether a wins over b: (hlc, signer did) greatest.
func beats(a, b cand) bool {
	if a.r.HLC != b.r.HLC {
		return a.r.HLC > b.r.HLC
	}
	return a.signer > b.signer
}

// keyBeats is beats with root-signed key records first.
func keyBeats(a, b cand) bool {
	if a.root != b.root {
		return a.root
	}
	return beats(a, b)
}

// Evaluate merges the held repositories (docs/cluster.md "State encoding and evaluation").
func Evaluate(g Genesis, src Repos, now time.Time, maxFuture time.Duration) *View {
	if maxFuture <= 0 {
		maxFuture = DefaultMaxFuture
	}
	id, _ := g.ID()
	v := &View{Genesis: g, ID: id, Root: g.RootKey}
	limit := HLC(now.Add(maxFuture).UnixMicro())

	// records returns the records of did evaluated under keys (revocation cutoffs).
	records := func(did string, keys map[string]*KeyState, rs *RepoState) *RepoRecords {
		head := src.Head(did)
		if rs != nil && head != nil {
			rs.Held, rs.Rev, rs.Used = head.Commit, head.Rev, head.Commit
		}
		k := keys[did]
		if k == nil || k.Revoked == nil {
			return head
		}
		if rs != nil {
			rs.Status = "revoked"
		}
		if head != nil && head.Commit == k.Revoked.After {
			return head
		}
		if at := src.At(did, k.Revoked.After); at != nil {
			if rs != nil {
				rs.Used = at.Commit
			}
			return at
		}
		if rs != nil {
			rs.Want, rs.Used, rs.Status = k.Revoked.After, cid.CID{}, "waiting"
		}
		// An earlier held commit is still within the accepted range.
		if head != nil {
			cut, err1 := ParseRev(k.Revoked.Rev)
			hr, err2 := ParseRev(head.Rev)
			if err1 == nil && err2 == nil && hr < cut {
				if rs != nil {
					rs.Used = head.Commit
				}
				return head
			}
		}
		return nil
	}

	// Keys: fixpoint from {root}.
	keys := map[string]*KeyState{g.RootKey: {DID: g.RootKey, Role: RoleRoot}}
	stable := false
	var rootOnly map[string]*KeyState
	for round := 0; round < maxRounds && !stable; round++ {
		win := map[string]cand{}
		for did, k := range keys {
			if k.Role != RoleRoot && k.Role != RoleAdmin {
				continue
			}
			rr := records(did, keys, nil)
			if rr == nil {
				continue
			}
			for _, r := range rr.Records {
				if r.Collection != NSKey || r.RKey == g.RootKey || r.HLC > limit {
					continue
				}
				c := cand{r: r, signer: did, root: did == g.RootKey}
				if old, ok := win[r.RKey]; !ok || keyBeats(c, old) {
					win[r.RKey] = c
				}
			}
		}
		next := map[string]*KeyState{g.RootKey: {DID: g.RootKey, Role: RoleRoot}}
		ro := map[string]*KeyState{g.RootKey: {DID: g.RootKey, Role: RoleRoot}}
		for did, c := range win {
			if c.r.Deleted {
				continue
			}
			kv := c.r.Value.(*Key)
			ks := &KeyState{DID: did, Role: kv.Role, Revoked: kv.Revoked, Signer: c.signer, HLC: c.r.HLC}
			if c.root {
				ks.Signer = ""
				ro[did] = ks
			}
			next[did] = ks
		}
		stable = sameKeys(keys, next)
		keys, rootOnly = next, ro
	}
	if !stable {
		keys = rootOnly
	}
	v.Keys = keys

	// Repositories and their records.
	v.Repos = map[string]*RepoState{}
	type signedRecs struct {
		did  string
		role string
		recs []*Record
	}
	var all []signedRecs
	dids := src.DIDs()
	slices.Sort(dids)
	for _, did := range dids {
		rs := &RepoState{DID: did, Status: "ok"}
		v.Repos[did] = rs
		k := keys[did]
		if k == nil {
			head := src.Head(did)
			if head != nil {
				rs.Held, rs.Rev = head.Commit, head.Rev
			}
			rs.Status = "unauthorized"
			continue
		}
		rs.Role = k.Role
		rr := records(did, keys, rs)
		if rr == nil {
			continue
		}
		v.Ignored += rr.Invalid
		all = append(all, signedRecs{did, k.Role, rr.Records})
	}
	for did, k := range keys {
		if v.Repos[did] == nil && k.Revoked != nil {
			v.Repos[did] = &RepoState{DID: did, Role: k.Role, Want: k.Revoked.After, Status: "waiting"}
		}
	}

	// Admin-scope records first (datasets decide writer scope), then heads/history.
	win := map[string]cand{}
	heads := map[string]map[string]cand{}
	consider := func(c cand) {
		if old, ok := win[c.r.Path()]; !ok || beats(c, old) {
			win[c.r.Path()] = c
		}
	}
	admin := func(role string) bool { return role == RoleRoot || role == RoleAdmin }
	for _, s := range all {
		for _, r := range s.recs {
			if r.HLC > v.MaxHLC && r.HLC <= limit {
				v.MaxHLC = r.HLC
			}
			if r.HLC > limit && r.Collection != NSGenesis {
				v.HeldBack++
				continue
			}
			c := cand{r: r, signer: s.did, root: s.did == g.RootKey}
			switch r.Collection {
			case NSGenesis, NSKey:
				// Genesis is fixed by the cluster ID; keys were merged above.
				if r.Collection == NSGenesis && !c.root {
					v.Ignored++
				}
			case NSMember, NSInvite, NSDataset, NSConfig, NSDrop:
				if admin(s.role) {
					consider(c)
				} else {
					v.Ignored++
				}
			case NSHead, NSHistory:
				// decided below, once datasets are merged
			default:
				v.Ignored++
			}
		}
	}
	datasets := map[string]*Dataset{}
	for _, c := range win {
		if c.r.Collection == NSDataset && !c.r.Deleted {
			datasets[c.r.RKey] = c.r.Value.(*Dataset)
		}
	}
	for _, s := range all {
		for _, r := range s.recs {
			if (r.Collection != NSHead && r.Collection != NSHistory) || r.HLC > limit {
				continue
			}
			ok := admin(s.role)
			if !ok && s.role == RoleWriter {
				if d := datasets[r.RKey]; d != nil && slices.Contains(d.Writers, s.did) {
					ok = true
				}
			}
			if !ok {
				v.Ignored++
				continue
			}
			c := cand{r: r, signer: s.did}
			consider(c)
			if r.Collection == NSHead {
				if heads[r.RKey] == nil {
					heads[r.RKey] = map[string]cand{}
				}
				heads[r.RKey][s.did] = c
			}
		}
	}

	v.Members = map[wire.PeerID]Signed[Member]{}
	v.Invites = map[string]Signed[Invite]{}
	v.Datasets = map[string]Signed[Dataset]{}
	v.History = map[string]Signed[cid.CID]{}
	v.Drop = map[string]Signed[cid.CID]{}
	v.Drops = map[string]Signed[Drop]{}
	v.Heads = map[string]map[string]Signed[Head]{}
	for _, c := range win {
		r := c.r
		switch r.Collection {
		case NSMember:
			id, _ := wire.ParsePeerID(r.RKey)
			v.Members[id] = signed[Member](c)
		case NSInvite:
			v.Invites[r.RKey] = signed[Invite](c)
		case NSDataset:
			v.Datasets[r.RKey] = signed[Dataset](c)
		case NSConfig:
			v.Config = signed[Config](c)
		case NSHistory:
			v.History[r.RKey] = Signed[cid.CID]{Value: &r.CID, Signer: c.signer, HLC: r.HLC, Deleted: r.Deleted}
		case NSDrop:
			v.Drop[r.RKey] = Signed[cid.CID]{Value: &r.CID, Signer: c.signer, HLC: r.HLC, Deleted: r.Deleted}
			v.Drops[r.RKey] = signed[Drop](c)
		}
	}
	for name, by := range heads {
		v.Heads[name] = map[string]Signed[Head]{}
		for did, c := range by {
			v.Heads[name][did] = signed[Head](c)
		}
	}
	return v
}

func signed[T any](c cand) Signed[T] {
	s := Signed[T]{Signer: c.signer, HLC: c.r.HLC, Deleted: c.r.Deleted}
	if !c.r.Deleted {
		s.Value, _ = c.r.Value.(*T)
	}
	return s
}

func sameKeys(a, b map[string]*KeyState) bool {
	return maps.EqualFunc(a, b, func(x, y *KeyState) bool {
		if x.Role != y.Role || (x.Revoked == nil) != (y.Revoked == nil) {
			return false
		}
		return x.Revoked == nil || *x.Revoked == *y.Revoked
	})
}

// Head returns the LWW-winning head of a dataset.
func (v *View) Head(name string) (Signed[Head], bool) {
	var best Signed[Head]
	found := false
	for did, h := range v.Heads[name] {
		if !found || h.HLC > best.HLC || (h.HLC == best.HLC && did > best.Signer) {
			best, found = h, true
		}
	}
	if found && best.Deleted {
		return best, false
	}
	return best, found
}

// DropPaths returns the paths a live drop record removes from a union collection.
func (v *View) DropPaths(name string) []string {
	d, ok := v.Drops[name]
	if !ok || d.Deleted || d.Value == nil {
		return nil
	}
	return d.Value.Paths
}

// Writers returns the authorized, unrevoked signers of name's heads in union rank
// order: the dataset's listed writers in list order, then other signers (admins and
// root) in DID order.
func (v *View) Writers(name string) []string {
	d, _ := v.Dataset(name)
	var out []string
	usable := func(did string) bool {
		k := v.Keys[did]
		h, ok := v.Heads[name][did]
		return k != nil && k.Revoked == nil && ok && !h.Deleted && h.Value != nil
	}
	if d != nil {
		for _, w := range d.Writers {
			if usable(w) && !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	var rest []string
	for did := range v.Heads[name] {
		if usable(did) && !slices.Contains(out, did) {
			rest = append(rest, did)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// IsMember reports whether id has a non-removed member record.
func (v *View) IsMember(id wire.PeerID) bool {
	m, ok := v.Members[id]
	return ok && !m.Deleted && m.Value.State != StateRemoved
}

// MemberIDs lists the non-removed members.
func (v *View) MemberIDs() []wire.PeerID {
	var out []wire.PeerID
	for id := range v.Members {
		if v.IsMember(id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// LiveInvite returns an unexpired, non-deleted invite.
func (v *View) LiveInvite(id string, now time.Time) (*Invite, bool) {
	in, ok := v.Invites[id]
	if !ok || in.Deleted || !now.Before(in.Value.ExpiresAt()) {
		return nil, false
	}
	return in.Value, true
}

// HasLiveInvites reports whether any secret invite can still admit.
func (v *View) HasLiveInvites(now time.Time) bool {
	for id := range v.Invites {
		if _, ok := v.LiveInvite(id, now); ok {
			return true
		}
	}
	return false
}

// Authorized reports whether did is a key whose repository is evaluated.
func (v *View) Authorized(did string) bool { return v.Keys[did] != nil }

// Dataset returns a live dataset record.
func (v *View) Dataset(name string) (*Dataset, bool) {
	d, ok := v.Datasets[name]
	if !ok || d.Deleted {
		return nil, false
	}
	return d.Value, true
}
