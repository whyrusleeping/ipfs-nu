package swarm

import (
	"context"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// signedAddresses is a bounded, freshness-limited lookup source for records
// already authenticated by identify. It does not persist virtual keys or grant
// relay/destination permission. Explicit hints/cache and the future DHT remain
// independent streaming sources, not dependencies of signed record publication.
func (s *Swarm) signedAddresses(ctx context.Context, id wire.PeerID, emit func(lookup.Address) bool) error {
	if s.identify == nil || s.cfg.Private || s.cfg.Isolated {
		return nil
	}
	info, ok := s.identify.Info(id)
	if !ok || info.Certified == nil || time.Since(info.Updated) > 10*time.Minute {
		return nil
	}
	for _, addr := range info.Certified.Addresses {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.host.IsLocalOnlyMultiaddr(addr) {
			continue
		}
		if host.IsCircuitAddress(addr) {
			if _, _, err := s.connectivity.authorized(addr, id); err != nil {
				continue
			}
		}
		if !emit(lookup.Address{Peer: id, Addr: addr, Source: "signed-identify", Confidence: .9, Seen: info.Updated, Expires: info.Updated.Add(10 * time.Minute)}) {
			break
		}
	}
	return nil
}
