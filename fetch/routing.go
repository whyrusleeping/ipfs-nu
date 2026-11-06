package fetch

import (
	"context"
	"errors"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Delegated adapts delegated provider routing (`/routing/v1/providers`, IPNI; D15 step 6)
// to Fetcher. Every `peer` record with a QUIC-v1 address is a candidate, whatever
// protocols it lists (Nu has no IPNI transport code); like Kademlia it installs the
// addresses Allow accepts (all when nil) as untrusted session hints, and /nu/have/1
// confirms the peer. Records naming Self are skipped.
type Delegated struct {
	Routing  *lookup.Providers
	Sessions *host.Sessions
	Self     wire.PeerID
	Allow    func(wire.Multiaddr) bool
}

func (d Delegated) Find(ctx context.Context, c cid.CID, found func(Peer)) error {
	if d.Routing == nil || d.Sessions == nil {
		return wire.ErrFormat
	}
	recs, err := d.Routing.Find(ctx, c.String())
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range recs {
		id, addrs, ok := r.QUIC()
		if !ok || id == d.Self {
			continue
		}
		hint := lookup.Hint{Peer: id}
		for _, a := range addrs {
			if d.Allow == nil || d.Allow(a) {
				hint.Addresses = append(hint.Addresses, a)
			}
		}
		if len(hint.Addresses) == 0 {
			continue
		}
		if e := d.Sessions.AddHints(ctx, []lookup.Hint{hint}); e != nil {
			errs = append(errs, e)
			continue
		}
		s, e := d.Sessions.Get(id)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		found(SessionPeer(s))
	}
	return errors.Join(errs...)
}
