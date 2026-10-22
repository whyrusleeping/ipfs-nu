package magicsock

import (
	"context"
	"net"

	quic "github.com/quic-go/quic-go"
)

// QUICConfigForPeer is the optional host socket-policy hook. Only virtual peers
// use the conservative inner budget and synchronous CID observer. Real-address
// connections (including outer relay QUIC) retain their normal configuration.
//
// This variant uses the proposed quic.Config.ConnectionIDTracer instead of a
// qlog tracer: quic-go reports exactly the changes it makes to its own
// Transport routing table for the connection (handshake CIDs, every
// NEW_CONNECTION_ID before it is sent, removal 3 PTO after retirement, removal
// of all remaining IDs on close). No qlog events are constructed. Callbacks are
// synchronous; CID rotation is part of receive routing correctness.
func (c *Conn) QUICConfigForPeer(addr net.Addr, base *quic.Config) *quic.Config {
	a, ok := addrPort(addr)
	if !ok || !IsVirtual(a) {
		return base
	}
	cfg := base.Clone()
	cfg.InitialPacketSize = PacketSize
	cfg.DisablePathMTUDiscovery = true
	old := base.ConnectionIDTracer
	cfg.ConnectionIDTracer = func(ctx context.Context, client bool, initial quic.ConnectionID) *quic.ConnectionIDTracer {
		var downstream *quic.ConnectionIDTracer
		if old != nil {
			downstream = old(ctx, client, initial)
		}
		c.mu.Lock()
		p := c.virtual[a]
		c.mu.Unlock()
		if p == nil {
			return downstream
		}
		o := &cidOwner{c: c, peer: p}
		return &quic.ConnectionIDTracer{
			AddedConnectionID: func(id quic.ConnectionID) {
				o.add(id)
				if downstream != nil && downstream.AddedConnectionID != nil {
					downstream.AddedConnectionID(id)
				}
			},
			RemovedConnectionID: func(id quic.ConnectionID) {
				o.remove(id)
				if downstream != nil && downstream.RemovedConnectionID != nil {
					downstream.RemovedConnectionID(id)
				}
			},
		}
	}
	return cfg
}

// cidOwner identifies one inner connection's routes, so a connection can only
// remove CIDs it registered, never another connection's colliding entry.
type cidOwner struct {
	c    *Conn
	peer *Peer
}

func (o *cidOwner) add(cid quic.ConnectionID) {
	c := o.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if cid.Len() == 0 || c.closed || c.peers[o.peer.id] != o.peer {
		return
	}
	if _, ok := c.cids[cid]; ok {
		return // already routed (by us, or by another connection: never steal it)
	}
	count := 0
	for _, r := range c.cids {
		if r.peer == o.peer {
			count++
		}
	}
	if count >= 64 {
		return
	}
	c.cids[cid] = cidRoute{peer: o.peer, owner: o}
}

func (o *cidOwner) remove(cid quic.ConnectionID) {
	c := o.c
	c.mu.Lock()
	if c.cids[cid].owner == o {
		delete(c.cids, cid)
	}
	c.mu.Unlock()
}
