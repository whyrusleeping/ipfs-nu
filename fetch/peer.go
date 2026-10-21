package fetch

import (
	"context"
	"errors"
	"sync"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
)

// Peer is a provider candidate the fetcher can open request streams to. Sessions (D9)
// implement it so requests follow the peer's best path; HostPeer is the minimal version
// over one host connection.
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
