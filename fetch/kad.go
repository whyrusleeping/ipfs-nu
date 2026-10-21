package fetch

import (
	"context"
	"errors"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Kademlia adapts Amino provider lookup to Fetcher. It installs provider
// addresses as untrusted session hints; authentication and private membership
// remain the session manager's responsibility.
type Kademlia struct {
	DHT      *kad.Service
	Sessions *host.Sessions
}

func (k Kademlia) Find(ctx context.Context, c cid.CID, found func(Peer)) error {
	if k.DHT == nil || k.Sessions == nil {
		return wire.ErrFormat
	}
	providers, err := k.DHT.FindProviders(ctx, c.Multihash().Bytes())
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range providers {
		addrs := make([]wire.Multiaddr, 0, len(p.Addrs))
		for _, raw := range p.Addrs {
			a, e := wire.MultiaddrFromBytes(raw, 1024)
			if e != nil {
				errs = append(errs, e)
				continue
			}
			a, e = lookup.NormalizeAddress(p.ID, a)
			if e != nil {
				errs = append(errs, e)
				continue
			}
			addrs = append(addrs, a)
		}
		if len(addrs) == 0 {
			continue
		}
		hint := lookup.Hint{Peer: p.ID, Addresses: addrs}
		if e := k.Sessions.AddHints(ctx, []lookup.Hint{hint}); e != nil {
			errs = append(errs, e)
			continue
		}
		s, e := k.Sessions.Get(p.ID)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		found(SessionPeer(s))
	}
	return errors.Join(errs...)
}
