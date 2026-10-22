package node

import (
	"slices"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
)

// memberOnly refuses public serving (`nu public add`) of a networked cluster dataset's
// head or retained version: cluster datasets are served to members only until the
// dataset sets `announce` (docs/cluster.md "Network modes"), which this build does not
// implement yet. Content protocols answer any peer that names a held CID (a CID is a
// capability); nothing advertises member-only roots (0069).
func (n *Local) memberOnly(root cid.CID) error {
	c, _ := n.netCluster()
	if c == nil {
		return nil
	}
	v := c.View()
	r := root.String()
	for ds := range v.Datasets {
		hit := slices.ContainsFunc(v.VersionHistory(ds), func(e cluster.VersionEntry) bool { return e.Root == r })
		for _, h := range v.Heads[ds] {
			hit = hit || !h.Deleted && h.Value != nil && h.Value.Target == r
		}
		if hit {
			return apiv0.Errorf(apiv0.CodeForbidden, "%s is a version of cluster dataset %q, which is served to members only", root, ds).
				WithAction("public serving of cluster datasets needs the dataset's `announce` policy (not in this build); share it with members via `" + nu.Name + " cluster dataset`")
		}
	}
	return nil
}
