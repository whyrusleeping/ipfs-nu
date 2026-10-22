package magicsock

import (
	"context"
	"net"
	"sync"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// QUICConfigForPeer is the optional host socket-policy hook. Only virtual peers
// use the conservative inner budget and synchronous CID observer. Real-address
// connections (including outer relay QUIC) retain their normal configuration.
// The observer consumes public quic-go qlog events, never serializes logs and
// registers NEW_CONNECTION_ID before it is put on the wire. Do not replace it
// with asynchronous logging: CID rotation is part of receive routing correctness.
func (c *Conn) QUICConfigForPeer(addr net.Addr, base *quic.Config) *quic.Config {
	a, ok := addrPort(addr)
	if !ok || !IsVirtual(a) {
		return base
	}
	cfg := base.Clone()
	cfg.InitialPacketSize = PacketSize
	cfg.DisablePathMTUDiscovery = true
	cfg.EnableDatagrams = false // inner connections never carry RFC 9221; only outer relay legs do (0019)
	old := base.Tracer
	cfg.Tracer = func(ctx context.Context, client bool, initial quic.ConnectionID) qlogwriter.Trace {
		c.mu.Lock()
		p := c.virtual[a]
		c.mu.Unlock()
		if p == nil {
			return nil
		}
		tr := &cidTrace{c: c, peer: p, seq: make(map[uint64]cidKey)}
		if old != nil {
			tr.downstream = old(ctx, client, initial)
		}
		return tr
	}
	return cfg
}

type cidTrace struct {
	c          *Conn
	peer       *Peer
	seq        map[uint64]cidKey
	refs       int
	closed     bool
	downstream qlogwriter.Trace
}
type cidRecorder struct {
	tr         *cidTrace
	downstream qlogwriter.Recorder
	once       sync.Once
}

func (t *cidTrace) SupportsSchemas(s string) bool {
	return s == qlog.EventSchema || t.downstream != nil && t.downstream.SupportsSchemas(s)
}
func (t *cidTrace) AddProducer() qlogwriter.Recorder {
	t.c.mu.Lock()
	t.refs++
	t.c.mu.Unlock()
	r := &cidRecorder{tr: t}
	if t.downstream != nil {
		r.downstream = t.downstream.AddProducer()
	}
	return r
}
func (t *cidTrace) bind(seq uint64, cid quic.ConnectionID) {
	if cid.Len() == 0 || t.closed || t.c.closed || t.c.peers[t.peer.id] != t.peer {
		return
	}
	key := cid
	if old, ok := t.c.cids[key]; ok && old.owner != t {
		return
	}
	if old := t.seq[seq]; old.Len() != 0 && old != key {
		delete(t.c.cids, old)
	}
	if _, ok := t.c.cids[key]; !ok {
		count := 0
		for _, r := range t.c.cids {
			if r.peer == t.peer {
				count++
			}
		}
		if count >= 64 {
			return
		}
	}
	t.seq[seq] = key
	t.c.cids[key] = cidRoute{peer: t.peer, owner: t}
}
func (r *cidRecorder) RecordEvent(event qlogwriter.Event) {
	t := r.tr
	c := t.c
	switch e := event.(type) {
	case qlog.PacketSent:
		if e.Header.SrcConnectionID.Len() > 0 {
			c.mu.Lock()
			t.bind(0, e.Header.SrcConnectionID)
			c.mu.Unlock()
		}
		for _, frame := range e.Frames {
			if f, ok := frame.Frame.(*qlog.NewConnectionIDFrame); ok {
				c.mu.Lock()
				t.bind(f.SequenceNumber, f.ConnectionID)
				c.mu.Unlock()
			}
		}
	case qlog.PacketReceived:
		for _, frame := range e.Frames {
			if f, ok := frame.Frame.(*qlog.RetireConnectionIDFrame); ok {
				c.mu.Lock()
				if key := t.seq[f.SequenceNumber]; key.Len() != 0 {
					if c.cids[key].owner == t {
						delete(c.cids, key)
					}
					delete(t.seq, f.SequenceNumber)
				}
				c.mu.Unlock()
			}
		}
	}
	if r.downstream != nil {
		r.downstream.RecordEvent(event)
	}
}
func (r *cidRecorder) Close() error {
	r.once.Do(func() {
		t := r.tr
		t.c.mu.Lock()
		t.refs--
		if t.refs == 0 {
			t.closed = true
			for _, key := range t.seq {
				if t.c.cids[key].owner == t {
					delete(t.c.cids, key)
				}
			}
		}
		t.c.mu.Unlock()
		if r.downstream != nil {
			_ = r.downstream.Close()
		}
	})
	return nil
}
