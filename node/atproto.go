package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/store"
)

// AtprotoNode is the atproto and CAR part of a node (0053): verified record fetch,
// repository snapshots and CAR import/export under walk profiles. Local and the HTTP
// client implement it; `ls`/`cat` on at:// targets go through Node.Ls/Cat.
type AtprotoNode interface {
	// AtRecord fetches at://<handle-or-did>/<collection>/<rkey> from the account's PDS
	// with its proof and returns the verified record.
	AtRecord(ctx context.Context, target string) (*apiv0.AtRecord, error)
	// AtSnapshot starts a snapshot operation (result: apiv0.AtSnapshot).
	AtSnapshot(ctx context.Context, req apiv0.AtSnapshotRequest) (*apiv0.Operation, error)
	// CarImport starts a CAR import operation (result: apiv0.CarImport).
	CarImport(ctx context.Context, req apiv0.CarImportRequest) (*apiv0.Operation, error)
	// CarExport streams a held root as a CAR under a walk profile ("" detects it).
	CarExport(ctx context.Context, target, profile string) (io.ReadCloser, error)
}

var _ AtprotoNode = (*Local)(nil)

// isAt reports whether a target is an at:// URI.
func isAt(t string) bool { return len(t) >= 5 && strings.EqualFold(t[:5], "at://") }

func (n *Local) atproto() (*atproto.Client, error) {
	if n.Atproto == nil {
		return nil, apiv0.NotImplemented("atproto fetching")
	}
	return n.Atproto, nil
}

// resolveAt resolves the URI's authority to a DID: a DID as given (no network), a handle
// through the atproto client.
func (n *Local) resolveAt(ctx context.Context, u atproto.URI) (string, error) {
	if u.IsDID() {
		return u.Authority, nil
	}
	c, err := n.atproto()
	if err != nil {
		return "", err
	}
	id, err := c.Resolve(ctx, u.Authority)
	if err != nil {
		return "", err
	}
	return id.DID, nil
}

func (n *Local) AtRecord(ctx context.Context, target string) (*apiv0.AtRecord, error) {
	u, err := atproto.ParseURI(target)
	if err != nil {
		return nil, err
	}
	if !u.IsRecord() {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s names a repository, not a record", u).
			WithAction("snapshot it with `" + nu.Name + " get " + u.String() + "`, or name a record: " + u.String() + "/<collection>/<rkey>")
	}
	if u.Rev != "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a live record fetch takes no @rev").
			WithAction("browse a held snapshot with `" + nu.Name + " cat " + u.String() + "`")
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	rec, err := c.GetRecord(ctx, u)
	if err != nil {
		return nil, err
	}
	// Keep the proof: the commit, path nodes and record are held (unpinned) so `dag get`
	// and `cat` work on them and the record stays re-verifiable while GC leaves them.
	for _, b := range rec.Proof {
		if err := n.Store.Put(b.CID, b.Data); err != nil {
			return nil, apiErr(err)
		}
	}
	js, err := content.RecordJSON(rec.Value)
	if err != nil {
		return nil, err
	}
	out := &apiv0.AtRecord{URI: rec.URI.String(), DID: rec.DID, Handle: rec.Handle, PDS: rec.PDS,
		Collection: u.Collection, RKey: u.RKey, CID: rec.CID.String(), Commit: rec.Commit.String(), Rev: rec.Rev,
		Value: js, Source: "pds", IdentityAge: rec.IdentityAge.Round(time.Second).String()}
	if rec.DataModel != nil {
		out.DataModel = rec.DataModel.Error()
	}
	return out, nil
}

// atCat is Cat for at:// targets: a record of a held snapshot as JSON, or, when no
// snapshot is held, a live verified fetch.
func (n *Local) atCat(ctx context.Context, target string) (io.ReadCloser, error) {
	u, err := atproto.ParseURI(target)
	if err != nil {
		return nil, err
	}
	if !u.IsRecord() {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a repository", u).WithAction("list it with `ls`, or cat a record inside it")
	}
	did, err := n.resolveAt(ctx, u)
	if err != nil {
		return nil, err
	}
	s, err := n.content.Snapshot(did, u.Rev)
	var ae *apiv0.Error
	if errors.As(err, &ae) && ae.Code == apiv0.CodeNotFound && u.Rev == "" {
		rec, err := n.AtRecord(ctx, u.String())
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(string(rec.Value) + "\n")), nil
	}
	if err != nil {
		return nil, err
	}
	_, data, err := n.content.Record(s, u.Collection, u.RKey)
	if err != nil {
		return nil, err
	}
	js, err := content.RecordJSON(data)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(js) + "\n")), nil
}

// atLs is Ls for at:// targets: the collections of a held snapshot, the records of a
// collection, or one record. Without a held snapshot (and no @rev) the repository is
// snapshotted first, like `ls <cid>` fetches content it does not hold.
func (n *Local) atLs(ctx context.Context, target string) (*apiv0.Listing, error) {
	u, err := atproto.ParseURI(target)
	if err != nil {
		return nil, err
	}
	did, err := n.resolveAt(ctx, u)
	if err != nil {
		return nil, err
	}
	s, err := n.content.Snapshot(did, u.Rev)
	var ae *apiv0.Error
	if errors.As(err, &ae) && ae.Code == apiv0.CodeNotFound && u.Rev == "" {
		if _, err := n.snapshot(ctx, atproto.URI{Authority: did}, false, nil); err != nil {
			return nil, err
		}
		s, err = n.content.Snapshot(did, "")
	}
	if err != nil {
		return nil, err
	}
	l := &apiv0.Listing{Target: atproto.URI{Authority: did, Rev: s.Rev, Collection: u.Collection, RKey: u.RKey}.String(), Entries: []apiv0.Entry{}}
	switch {
	case u.IsRecord():
		rc, data, err := n.content.Record(s, u.Collection, u.RKey)
		if err != nil {
			return nil, err
		}
		l.Entries = append(l.Entries, apiv0.Entry{Name: u.RKey, CID: rc.String(), Type: "record", Size: int64(len(data))})
	case u.Collection != "":
		recs, err := n.content.Records(s, u.Collection)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			l.Entries = append(l.Entries, apiv0.Entry{Name: r.RKey, CID: r.CID.String(), Type: "record", Size: r.Size})
		}
	default:
		cols, err := n.content.Collections(s)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			l.Entries = append(l.Entries, apiv0.Entry{Name: c.Name, CID: s.Commit.String(), Type: "collection", Size: int64(c.Count)})
		}
	}
	return l, nil
}

func (n *Local) AtSnapshot(ctx context.Context, req apiv0.AtSnapshotRequest) (*apiv0.Operation, error) {
	u, err := atproto.ParseURI(req.Target)
	if err != nil {
		return nil, err
	}
	if u.IsRecord() || u.Collection != "" || u.Rev != "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a snapshot takes a whole repository: at://<handle-or-did>").
			WithAction("fetch one record with `" + nu.Name + " get at://<did>/<collection>/<rkey>`")
	}
	if _, err := n.atproto(); err != nil {
		return nil, err
	}
	return n.start("at-snapshot", u.String(), func(o *op.Operation) (any, error) {
		return n.snapshot(o.Context(), u, req.Blobs, o)
	}), nil
}

// putStore records the first store error of a CAR read so budget refusals surface as
// such rather than as verification failures.
type putStore struct {
	st  *store.Store
	err error
}

func (p *putStore) Get(c cid.CID) ([]byte, error) { return p.st.Get(c) }
func (p *putStore) Put(c cid.CID, data []byte) error {
	err := p.st.Put(c, data)
	if err != nil && p.err == nil {
		p.err = err
	}
	return err
}

// snapshot fetches, verifies, stores and pins the repository of u. o may be nil (ls).
func (n *Local) snapshot(ctx context.Context, u atproto.URI, blobs bool, o *op.Operation) (*apiv0.AtSnapshot, error) {
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	stage := func(string) {}
	progress := func(int64, int64, string) {}
	if o != nil {
		stage, progress = o.Stage, o.Progress
	}
	stage("resolving")
	id, err := c.Resolve(ctx, u.Authority)
	if err != nil {
		return nil, err
	}
	stage("fetching")
	ps := &putStore{st: n.Store}
	snap, err := c.GetRepo(ctx, id, ps, func(b int64) { progress(b, 0, "bytes") })
	if ps.err != nil {
		return nil, apiErr(ps.err)
	}
	if err != nil {
		return nil, err
	}
	progress(snap.Bytes, snap.Bytes, "bytes")
	res := &apiv0.AtSnapshot{DID: snap.DID, Handle: snap.Handle, PDS: snap.PDS, Commit: snap.Commit.String(), Rev: snap.Rev,
		Records: snap.Stats.Records, Nodes: snap.Stats.Nodes, Bytes: snap.Bytes, NonConforming: snap.Stats.NonConforming, Profile: "atrepo"}
	pin := store.Pin{CID: snap.Commit, Mode: store.Recursive, Profile: "atrepo", Source: store.SourceExplicit, Name: content.SnapshotPinName(snap.DID, snap.Rev)}
	if blobs {
		stage("blobs")
		pin.Profile = "atrepo+blobs"
		res.Profile = pin.Profile
		var want []cid.CID
		if _, err := n.Store.Walk(ctx, pin, func(bc cid.CID, held bool) error {
			if !held && bc.Codec() == cid.Raw {
				want = append(want, bc)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		limit := c.Limits().MaxBlobs
		for i, bc := range want {
			if i >= limit {
				res.BlobsFailed += len(want) - i
				if o != nil {
					o.Log(fmt.Sprintf("stopped after %d blobs (limit)", limit))
				}
				break
			}
			progress(int64(i), int64(len(want)), "blobs")
			data, err := c.GetBlob(ctx, id, bc)
			switch {
			case errors.Is(err, atproto.ErrOversized):
				res.BlobsOversized++
				if o != nil {
					o.Log("skipped oversized blob " + bc.String())
				}
			case err != nil:
				if ctx.Err() != nil {
					return nil, err
				}
				res.BlobsFailed++
				if o != nil {
					o.Log("blob " + bc.String() + ": " + err.Error())
				}
			default:
				if err := n.Store.Put(bc, data); err != nil {
					return nil, apiErr(err)
				}
				res.Blobs++
			}
		}
	}
	stage("pinning")
	if err := n.Store.Pin(pin); err != nil {
		return nil, apiErr(err)
	}
	return res, nil
}

// CAR import and export.

// detectProfile picks the walk profile of a held root block by shape.
func detectProfile(c cid.CID, data []byte) string {
	if c.Codec() != cid.DRISL {
		return apiv0.ProfileDRISL
	}
	if _, err := atrepo.DecodeCommit(data); err == nil {
		return apiv0.ProfileAtrepo
	}
	if kids, err := tree.Links(c, data); err == nil && len(kids) > 0 {
		return apiv0.ProfileNuTree
	}
	return apiv0.ProfileDRISL
}

func checkProfile(p string) error {
	switch p {
	case "", apiv0.ProfileNuTree, apiv0.ProfileAtrepo, apiv0.ProfileDRISL:
		return nil
	}
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown walk profile %q", p).WithAction("use nu-tree, atrepo or drisl")
}

func (n *Local) CarImport(ctx context.Context, req apiv0.CarImportRequest) (*apiv0.Operation, error) {
	if err := checkProfile(req.Profile); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(req.Path) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "car import needs an absolute path").
			WithAction("the daemon reads the file itself, so the path must not depend on the caller's working directory")
	}
	var key atcrypto.PublicKey
	if req.Key != "" {
		k, err := atrepo.ParseKey(req.Key)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "key: %v", err).WithAction("give a did:key or a DID document publicKeyMultibase value")
		}
		key = k
	}
	return n.start("car-import", req.Path, func(o *op.Operation) (any, error) {
		return n.carImport(o, req, key)
	}), nil
}

func (n *Local) carImport(o *op.Operation, req apiv0.CarImportRequest, key atcrypto.PublicKey) (*apiv0.CarImport, error) {
	ctx := o.Context()
	f, err := os.Open(req.Path)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", req.Path, err).WithAction("check the path")
	}
	defer f.Close()
	o.Stage("reading")
	cr, err := car.NewReader(f, car.Options{})
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s: %v", req.Path, err)
	}
	roots := cr.Header().Roots
	if len(roots) != 1 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s has %d roots; Nu imports CARs with exactly one", req.Path, len(roots))
	}
	res := &apiv0.CarImport{Root: roots[0].String(), Profile: req.Profile}
	var rootData []byte
	last := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, dup, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s: block %d: %v", req.Path, res.Blocks, err).
				WithAction("the CAR is damaged or not a DASL CAR; every block must match its CID")
		}
		if dup {
			continue
		}
		if res.Blocks++; res.Blocks > atrepo.DefaultMaxBlocks {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s has more than %d blocks", req.Path, atrepo.DefaultMaxBlocks)
		}
		res.Bytes += int64(len(b.Data))
		if b.CID == roots[0] {
			rootData = b.Data
		}
		if err := n.Store.Put(b.CID, b.Data); err != nil {
			return nil, apiErr(err)
		}
		if time.Since(last) > 100*time.Millisecond {
			o.Progress(int64(res.Blocks), 0, "blocks")
			last = time.Now()
		}
	}
	o.Progress(int64(res.Blocks), 0, "blocks")
	res.Unreferenced = len(cr.Unreferenced())
	if rootData == nil {
		if rootData, err = n.Store.Get(roots[0]); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s does not contain its root %s", req.Path, roots[0])
		}
	}
	if res.Profile == "" {
		res.Profile = detectProfile(roots[0], rootData)
	}
	o.Stage("verifying")
	pin := store.Pin{CID: roots[0], Mode: store.Recursive, Profile: res.Profile, Source: store.SourceExplicit}
	switch res.Profile {
	case apiv0.ProfileAtrepo:
		com, err := atrepo.DecodeCommit(rootData)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "root %s is not an atproto v3 commit: %v", roots[0], err).
				WithAction("import it under another profile, or check the file")
		}
		opts := atrepo.VerifyOptions{DID: com.DID}
		switch {
		case key != nil:
			opts.Key, res.SignedBy, res.Signature = key, req.Key, "verified"
		case req.Offline:
			res.Signature = "unverified: offline import; structure verified, signature not checked"
		default:
			if c, err := n.atproto(); err == nil {
				if id, err := c.ResolveDID(ctx, com.DID); err == nil {
					opts.Key, res.SignedBy, res.Signature = id.Key, id.KeyMultibase, "verified"
				} else {
					res.Signature = "unverified: " + err.Error()
					o.Log("could not resolve " + com.DID + " to check the signature: " + err.Error())
				}
			}
		}
		repo, stats, err := atrepo.Verify(n.Store, roots[0], opts)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s does not verify as an atproto repository: %v", req.Path, err).
				WithAction("the commit, its signature or the MST/records are inconsistent; nothing was pinned")
		}
		res.DID, res.Rev, res.Records, res.NonConforming = repo.DID, repo.Head.Rev, stats.Records, stats.NonConforming
		res.Objects = stats.Records + stats.Nodes + 1
		pin.Name = content.SnapshotPinName(repo.DID, repo.Head.Rev)
	default:
		if res.Profile == apiv0.ProfileNuTree {
			// The root must be nu-tree structure (a tree header or a file node); other
			// DRISL has no links in this profile and would pin only itself.
			if kids, err := tree.Links(roots[0], rootData); err != nil || len(kids) == 0 {
				return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "root %s is not a Nu tree header or file node", roots[0]).
					WithAction("import it under the drisl or atrepo profile, or without --profile to detect one")
			}
		}
		st, err := n.Store.Walk(ctx, pin, nil)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "walking %s under %s: %v", roots[0], res.Profile, err)
		}
		res.Objects = st.Objects
		if !st.Complete() {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%s is incomplete under the %s profile: %d of %d objects are missing from the CAR",
				roots[0], res.Profile, st.Missing, st.Objects).WithAction("export it again from a node that holds all of it")
		}
	}
	if req.Pin {
		o.Stage("pinning")
		if pin.CID.Codec() == cid.Raw {
			pin.Mode, pin.Profile = store.Direct, ""
		}
		if err := n.Store.Pin(pin); err != nil {
			return nil, apiErr(err)
		}
		res.Pinned = true
	}
	return res, nil
}

func (n *Local) CarExport(ctx context.Context, target, profile string) (io.ReadCloser, error) {
	if err := checkProfile(profile); err != nil {
		return nil, err
	}
	root, err := parseCID(target)
	if err != nil {
		return nil, err
	}
	data, err := n.Store.Get(root)
	if errors.Is(err, store.ErrNotFound) {
		if _, ok, _ := n.Store.Has(root); ok && root.Codec() == cid.Raw {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a blob; CAR export takes a DRISL root", root).WithAction("read it with `cat`")
		}
		return nil, content.NotHeld(root)
	}
	if err != nil {
		return nil, err
	}
	if profile == "" {
		profile = detectProfile(root, data)
	}
	pin := store.Pin{CID: root, Mode: store.Recursive, Profile: profile, Source: store.SourceExplicit}
	st, err := n.Store.Walk(ctx, pin, nil)
	if err != nil {
		return nil, err
	}
	if !st.Complete() {
		return nil, missing(root, st.Missing, st.Objects)
	}
	pr, pw := io.Pipe()
	go func() {
		var err error
		if profile == apiv0.ProfileAtrepo {
			err = atrepo.Export(pw, n.Store, root)
		} else {
			err = n.exportPreOrder(pw, root, profile)
		}
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// exportPreOrder writes root and everything under it (nu-tree or drisl profile) in
// pre-order, each block once.
func (n *Local) exportPreOrder(w io.Writer, root cid.CID, profile string) error {
	links := car.LinksFunc(car.DRISLLinks)
	if profile == apiv0.ProfileNuTree {
		links = tree.Links
	}
	cw, err := car.NewWriter(w, car.Header{Roots: []cid.CID{root}})
	if err != nil {
		return err
	}
	return car.PreOrder([]cid.CID{root}, func(c cid.CID) ([]byte, error) {
		if c.Codec() == cid.Raw {
			b, err := n.Store.OpenBlob(c.Multihash())
			if err != nil {
				return nil, err
			}
			defer b.Close()
			if b.Size > cid.MaxWholeObject {
				return nil, fmt.Errorf("blob %s is %d bytes; CAR blocks are verified whole and limited to %d bytes", c, b.Size, cid.MaxWholeObject)
			}
			return io.ReadAll(io.NewSectionReader(b, 0, int64(b.Size)))
		}
		return n.Store.Get(c)
	}, links, cw.Put)
}
