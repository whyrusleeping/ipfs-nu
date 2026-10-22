package replica

import (
	"context"
	"math/rand/v2"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/store"
)

// Scrub verifies held objects of placed datasets against their CIDs (docs/cluster.md
// "Scrub and audit"): every object when sample is 0, else a random sample. A corrupt
// object (bit rot) is dropped, so the completeness walk reports the dataset incomplete
// and the member fetches it again from other members: detection leads to repair.
func (m *Manager) Scrub(ctx context.Context, sample int) apiv0.ScrubReport {
	rep := apiv0.ScrubReport{At: m.cfg.Cluster.Now(), Full: sample <= 0, Corrupt: []string{}}
	var objs []cid.CID
	seen := map[cid.Multihash]bool{}
	pins, _ := m.cfg.Store.Pins()
	for _, p := range pins {
		if p.Source != store.SourceCluster || len(p.Name) < 8 || p.Name[:8] != "dataset/" {
			continue
		}
		_, _ = m.cfg.Store.Walk(ctx, p, func(c cid.CID, held bool) error {
			if held && !seen[c.Multihash()] {
				seen[c.Multihash()] = true
				objs = append(objs, c)
			}
			return nil
		})
	}
	if sample > 0 && sample < len(objs) {
		rand.Shuffle(len(objs), func(i, j int) { objs[i], objs[j] = objs[j], objs[i] })
		objs = objs[:sample]
	}
	var corrupt []cid.Multihash
	for _, c := range objs {
		if ctx.Err() != nil {
			break
		}
		ok, err := m.cfg.Store.Verify(c)
		if err != nil {
			continue
		}
		rep.Checked++
		if info, held, _ := m.cfg.Store.Has(c); held {
			rep.Bytes += int64(info.Size)
		}
		if !ok {
			corrupt = append(corrupt, c.Multihash())
			rep.Corrupt = append(rep.Corrupt, c.String())
		}
	}
	if len(corrupt) > 0 {
		// Corrupt bytes protect nothing: drop them whatever pins reach them.
		if _, err := m.cfg.Store.Drop(ctx, corrupt, func(store.Pin) bool { return false }); err != nil {
			m.log.Warn("scrub: cannot drop corrupt objects", "err", err)
		}
		m.log.Warn("scrub: corrupt objects dropped for re-fetch", "count", len(corrupt))
		m.Kick()
	}
	m.mu.Lock()
	m.scrub = &rep
	m.mu.Unlock()
	return rep
}

// LastScrub returns the last scrub report, if any.
func (m *Manager) LastScrub() *apiv0.ScrubReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scrub == nil {
		return nil
	}
	r := *m.scrub
	return &r
}

// Sized is a blob with its size as the structure declares it.
type Sized struct {
	CID  cid.CID
	Size uint64
}

// OwnedBlobs lists the blobs of a held target that peer owns under dataset name's
// placement (audit sampling); the structure must be held here.
func (m *Manager) OwnedBlobs(name string, target cid.CID, owner func(*cluster.Placement, cid.Multihash) bool) []Sized {
	v := m.cfg.Cluster.View()
	d, ok := v.Dataset(name)
	if !ok {
		return nil
	}
	pl := cluster.NewPlacement(v, d.Policy, m.cfg.Self)
	var out []Sized
	var walk func(c cid.CID, size uint64, depth int)
	seen := map[cid.Multihash]bool{}
	walk = func(c cid.CID, size uint64, depth int) {
		if seen[c.Multihash()] || depth > 64 {
			return
		}
		seen[c.Multihash()] = true
		if c.Codec() == cid.Raw {
			if owner(pl, c.Multihash()) {
				out = append(out, Sized{c, size})
			}
			return
		}
		data, err := m.cfg.Store.Get(c)
		if err != nil {
			return
		}
		links, err := fetch.Links(c, data)
		if err != nil {
			return
		}
		for _, l := range links {
			walk(l.CID, l.Size, depth+1)
		}
	}
	walk(target, 0, 0)
	return out
}
