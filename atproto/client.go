package atproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
)

// Client fetches verified records, repository snapshots and blobs from PDSes.
type Client struct {
	*Resolver
	mu    sync.Mutex
	hosts map[string]chan struct{} // per-PDS-host concurrency
}

// New returns a client under cfg.
func New(cfg Config) *Client {
	return &Client{Resolver: NewResolver(cfg), hosts: map[string]chan struct{}{}}
}

// acquire takes a per-host slot (Limits.HostConcurrency) or fails when ctx ends.
func (c *Client) acquire(ctx context.Context, pds string) (func(), error) {
	u, err := url.Parse(pds)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	sem, ok := c.hosts[u.Host]
	if !ok {
		if len(c.hosts) >= maxCache {
			c.hosts = map[string]chan struct{}{}
		}
		sem = make(chan struct{}, c.cfg.Limits.HostConcurrency)
		c.hosts[u.Host] = sem
	}
	c.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Record is a verified record fetch.
type Record struct {
	URI    URI
	DID    string
	Handle string
	PDS    string
	// Commit and Rev identify the proven commit; CID is the record block.
	Commit cid.CID
	Rev    string
	CID    cid.CID
	// Value is the record's DRISL bytes.
	Value []byte
	// Proof is the proof CAR's blocks (commit, MST path, record), each verified by CID.
	Proof []car.Block
	// DataModel is the record's atproto data-model violation, if any (the record is
	// returned anyway; D21 keeps such records byte-exactly).
	DataModel error
	// IdentityAge is how old the cached identity used for verification was.
	IdentityAge time.Duration
}

// GetRecord resolves u's authority, fetches com.atproto.sync.getRecord and verifies the
// proof: commit signature with the DID document key, MST inclusion, record block. A
// signature failure retries once with a freshly resolved identity (key rotation).
func (c *Client) GetRecord(ctx context.Context, u URI) (*Record, error) {
	if !u.IsRecord() {
		return nil, invalid("%s names a repository, not a record", u)
	}
	id, err := c.Resolve(ctx, u.Authority)
	if err != nil {
		return nil, err
	}
	rec, err := c.getRecord(ctx, id, u)
	if errors.Is(err, atrepo.ErrSignature) && c.cfg.Now().Sub(id.Resolved) > time.Second {
		c.Forget(id.DID)
		if id, err = c.Resolve(ctx, u.Authority); err == nil {
			rec, err = c.getRecord(ctx, id, u)
		}
	}
	return rec, err
}

func (c *Client) getRecord(ctx context.Context, id *Identity, u URI) (*Record, error) {
	release, err := c.acquire(ctx, id.PDS)
	if err != nil {
		return nil, err
	}
	defer release()
	q := url.Values{"did": {id.DID}, "collection": {u.Collection}, "rkey": {u.RKey}}
	body, err := c.get(ctx, xrpc(id.PDS, "com.atproto.sync.getRecord", q), c.cfg.Limits.ProofBytes, nil)
	if err != nil {
		return nil, pdsError(id, "getRecord", err)
	}
	path := u.Path()
	p, err := atrepo.VerifyRecord(bytes.NewReader(body), path, atrepo.VerifyOptions{DID: id.DID, Key: id.Key, Now: c.cfg.Now(), MaxBlocks: atrepo.MaxProofBlocks})
	if err != nil {
		return nil, verifyError(id, fmt.Sprintf("record %s", path), err)
	}
	if !p.CID.Defined() {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s has no record %s (proven absent at rev %s)", id.DID, path, p.Commit.Rev)
	}
	blocks, err := carBlocks(body)
	if err != nil {
		return nil, verifyError(id, "proof CAR", err)
	}
	return &Record{URI: URI{Authority: id.DID, Collection: u.Collection, RKey: u.RKey}, DID: id.DID, Handle: id.Handle, PDS: id.PDS,
		Commit: p.CommitCID, Rev: p.Commit.Rev, CID: p.CID, Value: p.Data, Proof: blocks, DataModel: p.DataModel,
		IdentityAge: c.cfg.Now().Sub(id.Resolved)}, nil
}

func carBlocks(data []byte) ([]car.Block, error) {
	cr, err := car.NewReader(bytes.NewReader(data), car.Options{})
	if err != nil {
		return nil, err
	}
	var out []car.Block
	for {
		b, dup, err := cr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if !dup {
			out = append(out, b)
		}
	}
}

// Snapshot is a verified repository snapshot held in a store.
type Snapshot struct {
	DID, Handle, PDS string
	Commit           cid.CID
	Rev              string
	Repo             *atrepo.Repo
	Stats            atrepo.Stats
	// Bytes is the size of the CAR read.
	Bytes int64
}

// GetRepo fetches com.atproto.sync.getRepo for id into st (every block verified by CID
// as it arrives) and verifies the repository completely: commit signature, every MST
// node, every record. progress, if non-nil, receives the bytes read so far. On a
// verification failure the blocks already written stay in st (unpinned) and the error
// says what failed.
func (c *Client) GetRepo(ctx context.Context, id *Identity, st atrepo.Store, progress func(bytes int64)) (*Snapshot, error) {
	release, err := c.acquire(ctx, id.PDS)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	var count countingWriter
	count.w, count.progress = pw, progress
	fetchErr := make(chan error, 1)
	go func() {
		_, err := c.http.Fetch(ctx, xrpc(id.PDS, "com.atproto.sync.getRepo", url.Values{"did": {id.DID}}),
			func(int64) (io.Writer, error) { return &count, nil },
			httpsrc.Options{MaxBytes: c.cfg.Limits.RepoBytes, Timeout: c.cfg.Limits.RepoTimeout, IdleTimeout: c.cfg.Limits.Timeout})
		pw.CloseWithError(err) // nil closes cleanly
		fetchErr <- err
	}()
	root, readErr := atrepo.ReadCAR(pr, st, 0)
	pr.CloseWithError(readErr)
	if err := <-fetchErr; err != nil {
		return nil, pdsError(id, "getRepo", err)
	}
	if readErr != nil {
		return nil, verifyError(id, "repository CAR", readErr)
	}
	repo, stats, err := atrepo.Verify(st, root, atrepo.VerifyOptions{DID: id.DID, Key: id.Key, Now: c.cfg.Now()})
	if err != nil {
		return nil, verifyError(id, "repository "+root.String(), err)
	}
	return &Snapshot{DID: id.DID, Handle: id.Handle, PDS: id.PDS, Commit: root, Rev: repo.Head.Rev, Repo: repo, Stats: stats, Bytes: count.n}, nil
}

type countingWriter struct {
	w        io.Writer
	n        int64
	progress func(int64)
	last     time.Time
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	if c.progress != nil && time.Since(c.last) > 100*time.Millisecond {
		c.progress(c.n)
		c.last = time.Now()
	}
	return n, err
}

// ErrOversized reports a blob over the 8 MiB whole-object limit; it is skipped, not fatal.
var ErrOversized = errors.New("atproto: blob exceeds the 8 MiB whole-object limit")

// GetBlob fetches one blob of id's repository and verifies it against its SHA-256 raw
// CID. Blobs over cid.MaxWholeObject bytes fail with ErrOversized.
func (c *Client) GetBlob(ctx context.Context, id *Identity, b cid.CID) ([]byte, error) {
	if b.Codec() != cid.Raw || b.Hash() != cid.SHA256 {
		return nil, fmt.Errorf("%s is not a SHA-256 raw CID", b)
	}
	release, err := c.acquire(ctx, id.PDS)
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := c.get(ctx, xrpc(id.PDS, "com.atproto.sync.getBlob", url.Values{"did": {id.DID}, "cid": {b.String()}}), cid.MaxWholeObject, nil)
	if errors.Is(err, httpsrc.ErrTooLarge) {
		return nil, ErrOversized
	}
	if err != nil {
		return nil, pdsError(id, "getBlob", err)
	}
	if err := b.Verify(data); err != nil {
		return nil, verifyError(id, "blob "+b.String(), err)
	}
	return data, nil
}

func pdsError(id *Identity, method string, err error) error {
	code := apiv0.CodeUnavailable
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTP status 404"), strings.Contains(msg, "HTTP status 400"):
		code = apiv0.CodeNotFound // PDSes answer 400 RecordNotFound / RepoNotFound
	case errors.Is(err, httpsrc.ErrTooLarge):
		code = apiv0.CodeFailed
	case errors.Is(err, httpsrc.ErrPolicy):
		code = apiv0.CodeForbidden
	}
	return apiv0.Errorf(code, "%s from %s for %s: %v", method, id.PDS, id.DID, err).
		WithAction("check the account's PDS and your network; the identity was resolved from its DID document")
}

func verifyError(id *Identity, what string, err error) error {
	return apiv0.Errorf(apiv0.CodeFailed, "%s of %s from %s does not verify: %v", what, id.DID, id.PDS, err).
		WithAction("the PDS served data that does not match the DID document's signing key or the repository structure; nothing was trusted")
}
