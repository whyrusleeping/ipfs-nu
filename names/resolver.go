package names

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
)

// Resolution is a verified answer for a name (docs/names.md D6): the record's target
// and the proof that established it. Resolved is when the proof was fetched; a cached
// answer carries its age and Source "cache".
type Resolution struct {
	DID, Handle, Name string
	Target            cid.CID
	Kind              string
	Size              int64
	Note              string
	UpdatedAt         string
	// Rev and Commit identify the signed commit of the proof; RecordCID the record block.
	Rev       string
	Commit    cid.CID
	RecordCID cid.CID
	Proof     []car.Block
	Resolved  time.Time
	Source    string // "pds" or "cache"
	// IdentityAge is how old the DID document used for verification was.
	IdentityAge time.Duration
}

// URI is the at:// form of the resolved name.
func (r *Resolution) URI() string {
	return "at://" + r.DID + "/" + CollectionName + "/" + r.Name
}

// Age is how old the answer is at now.
func (r *Resolution) Age(now time.Time) time.Duration { return now.Sub(r.Resolved) }

// ErrDeleted reports a name proven absent in the publisher's repository.
type ErrDeleted struct {
	DID, Name, Rev string
}

func (e *ErrDeleted) Error() string {
	return fmt.Sprintf("%s has no name %q (proven absent at rev %s)", e.DID, e.Name, e.Rev)
}

// DefaultTTL is how long a resolution answers from the cache (D6: proofs establish
// authenticity, not freshness; stale answers are acceptable and labeled).
const DefaultTTL = 5 * time.Minute

// Resolver resolves names through an atproto client with a bounded cache.
type Resolver struct {
	c   *atproto.Client
	ttl time.Duration
	now func() time.Time
	mu  sync.Mutex
	// cache is keyed by did/name; deleted names are cached as nil answers with a rev.
	cache map[string]*entry
}

type entry struct {
	res     *Resolution
	deleted *ErrDeleted
	at      time.Time
}

const maxCache = 4096

// NewResolver returns a resolver over c (ttl 0: DefaultTTL; now nil: time.Now).
func NewResolver(c *atproto.Client, ttl time.Duration, now func() time.Time) *Resolver {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Resolver{c: c, ttl: ttl, now: now, cache: map[string]*entry{}}
}

// Options select freshness. MaxAge > 0 accepts a cached answer at most that old; 0
// uses the resolver's TTL; Fresh forces a PDS fetch; Offline answers only from the
// cache (any age) and fails with unavailable otherwise.
type Options struct {
	MaxAge  time.Duration
	Fresh   bool
	Offline bool
}

// Resolve resolves authority/name: handle → DID → PDS → verified getRecord proof.
// A cache hit needs no name-plane request at all (not even identity resolution when
// the authority is a DID or a handle seen before).
func (r *Resolver) Resolve(ctx context.Context, authority, name string, o Options) (*Resolution, error) {
	if err := ValidName(name); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	did := authority
	if !strings.HasPrefix(authority, "did:") {
		// A cached answer may be keyed by handle too (no identity request on a hit).
		if res, derr, ok := r.hit(authority+"/"+name, o); ok {
			return res, derr
		}
		if o.Offline {
			return nil, apiv0.Errorf(apiv0.CodeUnavailable, "%s/%s is not in the name cache", authority, name).
				WithAction("resolve it online once, or use the DID")
		}
		id, err := r.c.Resolve(ctx, authority)
		if err != nil {
			return nil, err
		}
		did = id.DID
	}
	key := did + "/" + name
	if res, derr, ok := r.hit(key, o); ok {
		return res, derr
	}
	if o.Offline {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "%s is not in the name cache", key).WithAction("resolve it online once")
	}
	rec, err := r.c.GetRecord(ctx, atproto.URI{Authority: did, Collection: CollectionName, RKey: name})
	if err != nil {
		if rev := atproto.AbsentRev(err); rev != "" {
			d := &ErrDeleted{DID: did, Name: name, Rev: rev}
			r.put(key, authority, name, &entry{deleted: d, at: r.now()})
			return nil, d
		}
		return nil, err
	}
	n, err := DecodeName(rec.Value)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s/%s: %v", did, name, err).
			WithAction("the publisher's record is not a Nu name record; nothing was trusted")
	}
	target, err := n.Validate()
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "%s/%s: invalid name record: %v", did, name, err)
	}
	res := &Resolution{DID: rec.DID, Handle: rec.Handle, Name: name, Target: target, Kind: n.Kind, Size: n.Size, Note: n.Note,
		UpdatedAt: n.UpdatedAt, Rev: rec.Rev, Commit: rec.Commit, RecordCID: rec.CID, Proof: rec.Proof, Resolved: r.now(), Source: "pds",
		IdentityAge: rec.IdentityAge}
	r.put(key, authority, name, &entry{res: res, at: res.Resolved})
	return res, nil
}

func (r *Resolver) hit(key string, o Options) (*Resolution, error, bool) {
	if o.Fresh {
		return nil, nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[key]
	if !ok {
		return nil, nil, false
	}
	age := r.now().Sub(e.at)
	max := r.ttl
	if o.MaxAge > 0 {
		max = o.MaxAge
	}
	if !o.Offline && age > max {
		return nil, nil, false
	}
	if e.deleted != nil {
		return nil, e.deleted, true
	}
	c := *e.res
	c.Source = "cache"
	return &c, nil, true
}

func (r *Resolver) put(key, authority, name string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxCache {
		r.cache = map[string]*entry{}
	}
	r.cache[key] = e
	if !strings.HasPrefix(authority, "did:") {
		r.cache[authority+"/"+name] = e
	}
}

// Forget drops cached answers for did/name (and every handle alias of it).
func (r *Resolver) Forget(did, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.cache {
		if k == did+"/"+name || (e.res != nil && e.res.DID == did && e.res.Name == name) || (e.deleted != nil && e.deleted.DID == did && e.deleted.Name == name) {
			delete(r.cache, k)
		}
	}
}

// Cached returns the cached answer for did/name regardless of age, if any.
func (r *Resolver) Cached(did, name string) (*Resolution, bool) {
	res, derr, ok := r.hit(did+"/"+name, Options{Offline: true})
	return res, ok && derr == nil
}

// Client exposes the underlying atproto client.
func (r *Resolver) Client() *atproto.Client { return r.c }
