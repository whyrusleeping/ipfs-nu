package cluster

import (
	"fmt"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/store"
)

// VerifyHeads checks every state repository named in the state file against its did:key
// (commit signature, MST and every record) and (re)pins it, as after restoring a backup
// onto a clean machine (docs/operations.md "Backup, export and recovery"). It fails on
// the first head that is missing from the store or does not verify; nothing is pinned
// for that head. It returns the verified heads.
func (c *Cluster) VerifyHeads() ([]HeadInfo, error) {
	c.mu.Lock()
	heads := make(map[string]string, len(c.file.Heads))
	for did, s := range c.file.Heads {
		heads[did] = s
	}
	c.mu.Unlock()
	var out []HeadInfo
	for did, s := range heads {
		commit, err := cid.Parse(s)
		if err != nil {
			return out, fmt.Errorf("cluster: head of %s: %w", did, err)
		}
		pub, err := parseP256(did)
		if err != nil {
			return out, fmt.Errorf("cluster: head of %s: %w", did, err)
		}
		r, _, err := atrepo.Verify(c.opts.Store, commit, atrepo.VerifyOptions{DID: did, Key: pub, Now: c.opts.now()})
		if err != nil {
			return out, fmt.Errorf("cluster: state repository %s at %s does not verify: %w", did, commit, err)
		}
		pin := store.Pin{CID: commit, Mode: store.Recursive, Profile: "atrepo", Source: store.SourceCluster, Name: "state/" + c.ID.String() + "/" + did}
		if err := c.opts.Store.Pin(pin); err != nil {
			return out, err
		}
		out = append(out, HeadInfo{DID: did, Rev: r.Head.Rev, Commit: commit})
	}
	c.mu.Lock()
	c.cache = map[cid.CID]*RepoRecords{}
	c.evaluateLocked()
	c.mu.Unlock()
	return out, nil
}
