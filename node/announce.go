package node

import (
	"github.com/whyrusleeping/nu/core/cid"
)

// Announcer is what a Net may implement to provide freshly pinned roots on the DHT
// (node/swarm does; D15 decides which CIDs beneath the root are announced).
type Announcer interface {
	Announce(root cid.CID)
}

// pinned records an explicit pin's announce policy and, unless noAnnounce, asks the
// network to provide it. `--no-announce` is remembered in the repo (repo.NoAnnounceFile)
// so the periodic provide cycle skips the root; an announced pin of the same root clears
// the mark.
func (n *Local) pinned(root cid.CID, noAnnounce bool) error {
	if err := n.Repo.SetNoAnnounce(root.String(), noAnnounce); err != nil {
		return err
	}
	if noAnnounce {
		return nil
	}
	if a, ok := n.Net.(Announcer); ok {
		a.Announce(root)
	}
	return nil
}
