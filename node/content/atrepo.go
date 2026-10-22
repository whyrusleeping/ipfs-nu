package content

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/store"
)

// Held atproto repository snapshots (D21 "Using atproto data"). A snapshot is a
// recursive pin of a verified commit under the atrepo (or atrepo+blobs) profile whose
// name is SnapshotPinName(did, rev); browsing reads the held MST and records without
// re-verifying (verification happened when the snapshot was stored).

// SnapshotPinName is the pin name of a held snapshot: at://<did>@<rev>.
func SnapshotPinName(did, rev string) string { return "at://" + did + "@" + rev }

// Snapshot is a held repository snapshot.
type Snapshot struct {
	DID     string
	Rev     string
	Commit  cid.CID
	Profile string
	Repo    *atrepo.Repo
}

// Collection is one record collection of a snapshot.
type Collection struct {
	Name  string
	Count int
}

// Record is one record of a snapshot.
type Record struct {
	RKey string
	CID  cid.CID
	Size int64
}

// Snapshots lists the held snapshots of did, newest rev first (TIDs sort as strings).
func (c *Content) Snapshots(did string) ([]Snapshot, error) {
	pins, err := c.st.Pins()
	if err != nil {
		return nil, err
	}
	prefix := SnapshotPinName(did, "")
	var out []Snapshot
	seen := map[cid.CID]bool{}
	for _, p := range pins {
		if p.Mode != store.Recursive || !strings.HasPrefix(p.Profile, "atrepo") || seen[p.CID] {
			continue
		}
		switch {
		case strings.HasPrefix(p.Name, prefix):
			seen[p.CID] = true
			out = append(out, Snapshot{DID: did, Rev: strings.TrimPrefix(p.Name, prefix), Commit: p.CID, Profile: p.Profile})
		case strings.HasPrefix(p.Name, "dataset/"):
			// An atrepo dataset's head or retained version (0054): the commit names its
			// account and rev.
			if data, err := c.st.Get(p.CID); err == nil {
				if com, err := atrepo.DecodeCommit(data); err == nil && com.DID == did {
					seen[p.CID] = true
					out = append(out, Snapshot{DID: did, Rev: com.Rev, Commit: p.CID, Profile: p.Profile})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rev > out[j].Rev })
	return out, nil
}

// Snapshot opens the held snapshot of did at rev ("" for the newest).
func (c *Content) Snapshot(did, rev string) (*Snapshot, error) {
	snaps, err := c.Snapshots(did)
	if err != nil {
		return nil, err
	}
	for i := range snaps {
		if rev == "" || snaps[i].Rev == rev {
			s := &snaps[i]
			if s.Repo, err = atrepo.Open(c.st, s.Commit); err != nil {
				return nil, snapshotErr(s.Commit, err)
			}
			return s, nil
		}
	}
	msg := "no snapshot of " + did + " is held"
	if rev != "" {
		msg = "no snapshot of " + did + " at rev " + rev + " is held"
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s", msg).
		WithAction("fetch one with `get at://" + did + "`, or import a CAR with `car import --profile atrepo`")
}

func snapshotErr(c cid.CID, err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, mst.ErrNotFound) {
		return apiv0.Errorf(apiv0.CodeNotFound, "part of snapshot %s is no longer held", c).
			WithAction("fetch the snapshot again").WithDetail(MissingDetail, c.String())
	}
	return err
}

// Collections lists the snapshot's collections with record counts, in name order. It
// walks the key space collection by collection (keys are <collection>/<rkey>, and '0'
// is the byte after '/').
func (c *Content) Collections(s *Snapshot) ([]Collection, error) {
	var out []Collection
	from := ""
	for {
		var first string
		found := false
		err := s.Repo.Tree.Walk(from, func(k string, _ cid.CID) (bool, error) {
			first, found = k, true
			return false, nil
		})
		if err != nil {
			return nil, snapshotErr(s.Commit, err)
		}
		if !found {
			return out, nil
		}
		coll, _, _ := strings.Cut(first, "/")
		col := Collection{Name: coll}
		err = s.Repo.Tree.WalkPrefix(coll+"/", func(string, cid.CID) (bool, error) {
			col.Count++
			return true, nil
		})
		if err != nil {
			return nil, snapshotErr(s.Commit, err)
		}
		out = append(out, col)
		from = coll + "0"
	}
}

// Records lists a collection's records in rkey order.
func (c *Content) Records(s *Snapshot, collection string) ([]Record, error) {
	var out []Record
	err := s.Repo.Tree.WalkPrefix(collection+"/", func(k string, v cid.CID) (bool, error) {
		r := Record{RKey: strings.TrimPrefix(k, collection+"/"), CID: v}
		if info, ok, err := c.st.Has(v); err != nil {
			return false, err
		} else if ok {
			r.Size = int64(info.Size)
		}
		out = append(out, r)
		return true, nil
	})
	if err != nil {
		return nil, snapshotErr(s.Commit, err)
	}
	return out, nil
}

// Record returns one record's CID and DRISL bytes.
func (c *Content) Record(s *Snapshot, collection, rkey string) (cid.CID, []byte, error) {
	rc, data, ok, err := s.Repo.Get(collection + "/" + rkey)
	if err != nil {
		return cid.CID{}, nil, snapshotErr(s.Commit, err)
	}
	if !ok {
		return cid.CID{}, nil, apiv0.Errorf(apiv0.CodeNotFound, "snapshot %s@%s has no record %s/%s", s.DID, s.Rev, collection, rkey)
	}
	return rc, data, nil
}

// RecordJSON renders a record block as atproto-style JSON.
func RecordJSON(data []byte) ([]byte, error) {
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, err
	}
	return drisl.ToJSON(v)
}

// gateway.AtContent and AtDatasetContent.

var (
	_ gateway.AtContent        = gw{}
	_ gateway.AtDatasetContent = gw{}
)

// AtLocation redirects /c/ of an atrepo dataset root to /at/<did>@<rev>/ (0054).
func (g gw) AtLocation(ctx context.Context, root string) (string, bool) {
	c, err := cid.Parse(root)
	if err != nil || c.Codec() != cid.DRISL {
		return "", false
	}
	data, err := g.c.st.Get(c)
	if err != nil {
		return "", false
	}
	com, err := atrepo.DecodeCommit(data)
	if err != nil {
		return "", false
	}
	return "/at/" + com.DID + "@" + com.Rev + "/", true
}

func (g gw) AtSnapshot(ctx context.Context, did, rev string) (*gateway.AtSnapshot, error) {
	s, err := g.c.Snapshot(did, rev)
	if err != nil {
		return nil, err
	}
	return &gateway.AtSnapshot{DID: s.DID, Rev: s.Rev, Commit: s.Commit.String()}, nil
}

func (g gw) open(s *gateway.AtSnapshot) (*Snapshot, error) {
	return g.c.Snapshot(s.DID, s.Rev)
}

func (g gw) AtCollections(ctx context.Context, s *gateway.AtSnapshot) ([]gateway.AtCollection, error) {
	snap, err := g.open(s)
	if err != nil {
		return nil, err
	}
	cols, err := g.c.Collections(snap)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.AtCollection, 0, len(cols))
	for _, c := range cols {
		out = append(out, gateway.AtCollection{Name: c.Name, Count: c.Count})
	}
	return out, nil
}

func (g gw) AtRecords(ctx context.Context, s *gateway.AtSnapshot, collection string) ([]gateway.AtRecord, error) {
	snap, err := g.open(s)
	if err != nil {
		return nil, err
	}
	recs, err := g.c.Records(snap, collection)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.AtRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, gateway.AtRecord{RKey: r.RKey, CID: r.CID.String(), Size: r.Size})
	}
	return out, nil
}

func (g gw) AtRecordJSON(ctx context.Context, s *gateway.AtSnapshot, collection, rkey string) ([]byte, string, error) {
	snap, err := g.open(s)
	if err != nil {
		return nil, "", err
	}
	rc, data, err := g.c.Record(snap, collection, rkey)
	if err != nil {
		return nil, "", err
	}
	js, err := RecordJSON(data)
	if err != nil {
		return nil, "", err
	}
	return js, rc.String(), nil
}
