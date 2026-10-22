package fetch

import (
	"context"
	"errors"
	"sync"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
)

// Peer is a provider candidate the fetcher can open request streams to. SessionPeer
// follows the peer's best path (D9); HostPeer only uses existing host connections.
type Peer interface {
	ID() wire.PeerID
	// Open opens a new stream for one request (see proto.Opener).
	Open(ctx context.Context, protocol string) (proto.Stream, error)
}

type hostPeer struct {
	id   wire.PeerID
	open proto.Opener
}

func (p hostPeer) ID() wire.PeerID { return p.id }
func (p hostPeer) Open(ctx context.Context, protocol string) (proto.Stream, error) {
	return p.open(ctx, protocol)
}

// HostPeer is a peer reached over h's existing connection to id.
func HostPeer(h *host.Host, id wire.PeerID) Peer {
	return hostPeer{id: id, open: proto.HostOpener(h, id)}
}

// SessionPeer opens each request through s, dialing on demand and reconnecting after
// connection loss. It never retains a connection or replays request bytes; the fetcher
// resumes verified ranges on a new stream. The caller owns the session manager's lifetime.
// Forget invalidates this peer just as it invalidates s.
func SessionPeer(s *host.Session) Peer {
	return hostPeer{id: s.PeerID(), open: func(ctx context.Context, protocol string) (proto.Stream, error) {
		stream, err := s.OpenStream(ctx, protocol)
		if err != nil {
			return nil, err
		}
		return sessionStream{stream}, nil
	}}
}

// Providers finds candidate peers for content (D15 sources: link hints, publisher nodes,
// swarms, connected/LAN candidates, routing). Find calls found for each candidate as it is
// discovered and returns when the source is exhausted. Candidates are confirmed with
// /nu/have/1 before data is requested, so sources may over-approximate.
type Providers interface {
	Find(ctx context.Context, c cid.CID, found func(Peer)) error
}

// Static always offers the same peers (explicit or link hints).
type Static []Peer

func (s Static) Find(_ context.Context, _ cid.CID, found func(Peer)) error {
	for _, p := range s {
		found(p)
	}
	return nil
}

// Hinted offers session-backed peers from parsed link hints (e.g. lookup.ParseHints).
// Find installs addresses without dialing; /nu/have/1 opens the first connection.
// Hints confer no private membership: Sessions must authorize those peers separately.
// Invalid/rejected hints are reported while other candidates remain usable.
// The caller must not mutate Hints during Find and owns Sessions' lifetime.
type Hinted struct {
	Sessions *host.Sessions
	Hints    []lookup.Hint
}

func (h Hinted) Find(ctx context.Context, _ cid.CID, found func(Peer)) error {
	var errs []error
	for _, hint := range h.Hints {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := h.Sessions.AddHints(ctx, []lookup.Hint{hint}); err != nil {
			errs = append(errs, err)
			continue
		}
		s, err := h.Sessions.Get(hint.Peer)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		found(SessionPeer(s))
	}
	return errors.Join(errs...)
}

// Connected offers every peer h is connected to. Use it only where connected peers are
// candidates by policy (LAN, explicit dev setups); D15 never broadcasts to all peers.
type Connected struct{ Host *host.Host }

func (c Connected) Find(_ context.Context, _ cid.CID, found func(Peer)) error {
	seen := map[wire.PeerID]bool{}
	for _, conn := range c.Host.Connections() {
		if id := conn.PeerID(); !seen[id] {
			seen[id] = true
			found(HostPeer(c.Host, id))
		}
	}
	return nil
}

// Multi queries several sources concurrently and merges their candidates.
type Multi []Providers

func (m Multi) Find(ctx context.Context, c cid.CID, found func(Peer)) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, len(m))
	for i, src := range m {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = src.Find(ctx, c, func(p Peer) { mu.Lock(); defer mu.Unlock(); found(p) })
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
