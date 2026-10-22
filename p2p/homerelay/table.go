package homerelay

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// TableProtocol exchanges a node's relay latency table for pair-aware relay
// choice (D11c §5): the requester writes max_version:u8 (>= 1) and half-closes;
// the responder writes `version=1:u8 | count:u8 (0-16) | count × (rtt_ms:u16 |
// flags:u8 | peer_len:u8 | peer)` then EOF. rtt_ms is 1-65534 measured or 65535
// unmeasured (0 is rejected); flags must be zero in v1. Entries are relays where the responder currently HOLDS a
// reservation (so the requester can CONNECT through them); nothing else is
// implied, and an entry is never authorization to use an unknown relay.
const (
	TableProtocol = "/nu/relays/1"
	TableVersion  = 1
	MaxTable      = 16
	rttUnknown    = 0xffff
)

var ErrTable = errors.New("homerelay: invalid relay table")

type TableEntry struct {
	Peer wire.PeerID
	RTT  time.Duration // 0 = unmeasured
}

// Table lists the held (home/backup) reservations with their latest RTT.
func (m *Manager) Table() []TableEntry {
	s := m.Snapshot()
	var out []TableEntry
	for _, id := range []wire.PeerID{s.Home, s.Backup} {
		for _, r := range s.Relays {
			if id != "" && r.Peer == id && r.Expires.After(time.Now()) {
				out = append(out, TableEntry{Peer: r.Peer, RTT: r.RTT})
			}
		}
	}
	return out
}

func EncodeTable(entries []TableEntry) ([]byte, error) {
	if len(entries) > MaxTable {
		return nil, ErrTable
	}
	b := []byte{TableVersion, byte(len(entries))}
	for _, e := range entries {
		if _, err := wire.PeerIDFromBytes(e.Peer.Bytes()); err != nil || len(e.Peer.Bytes()) > 64 {
			return nil, ErrTable
		}
		ms := uint16(rttUnknown)
		if e.RTT > 0 {
			ms = uint16(min(e.RTT.Milliseconds(), rttUnknown-1))
			if ms == 0 {
				ms = 1
			}
		}
		b = binary.BigEndian.AppendUint16(b, ms)
		b = append(b, 0, byte(len(e.Peer.Bytes())))
		b = append(b, e.Peer.Bytes()...)
	}
	return b, nil
}
func DecodeTable(b []byte) ([]TableEntry, error) {
	if len(b) < 2 || b[0] != TableVersion || int(b[1]) > MaxTable {
		return nil, ErrTable
	}
	out := make([]TableEntry, 0, b[1])
	rest := b[2:]
	seen := map[wire.PeerID]bool{}
	for range int(b[1]) {
		if len(rest) < 4 {
			return nil, ErrTable
		}
		ms := binary.BigEndian.Uint16(rest)
		n := int(rest[3])
		if ms == 0 || rest[2] != 0 || n == 0 || n > 64 || len(rest) < 4+n { // rtt_ms 0 is not canonical
			return nil, ErrTable
		}
		id, err := wire.PeerIDFromBytes(rest[4 : 4+n])
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, ErrTable
		}
		seen[id] = true
		e := TableEntry{Peer: id}
		if ms != rttUnknown {
			e.RTT = time.Duration(ms) * time.Millisecond
		}
		out = append(out, e)
		rest = rest[4+n:]
	}
	if len(rest) != 0 {
		return nil, ErrTable
	}
	return out, nil
}

// ServeTable answers TableProtocol with the owner's current table. Owners that
// restrict who may connect (private mode) gate the host, not this handler.
func ServeTable(h *host.Host, table func() []TableEntry) error {
	return h.Handle(TableProtocol, 16, func(st *host.Stream) {
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		var v [1]byte
		if _, err := io.ReadFull(st, v[:]); err != nil || v[0] == 0 {
			st.Reset()
			return
		}
		b, err := EncodeTable(table())
		if err != nil {
			b = []byte{TableVersion, 0}
		}
		if _, err := st.Write(b); err != nil {
			st.Reset()
		}
	})
}

// FetchTable retrieves a peer's relay table over any authenticated connection.
func FetchTable(ctx context.Context, h *host.Host, peer wire.PeerID) ([]TableEntry, error) {
	st, err := h.OpenStream(ctx, peer, TableProtocol)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = st.SetDeadline(deadline)
	if _, err = st.Write([]byte{TableVersion}); err != nil {
		st.Reset()
		return nil, err
	}
	_ = st.CloseWrite()
	b, err := io.ReadAll(io.LimitReader(st, 2+MaxTable*(4+64)+1))
	if err != nil {
		st.Reset()
		return nil, err
	}
	return DecodeTable(b)
}

// Choose returns the relay minimising RTT(local→relay)+RTT(relay→peer) among
// relays present in BOTH tables with measured RTTs, and whether it beats the
// current relay by the switching hysteresis (both 20% and 5 ms better, or the
// current relay is absent/unmeasured). Unknown relays are never chosen.
func Choose(local, remote []TableEntry, current wire.PeerID) (best wire.PeerID, sum time.Duration, better bool) {
	mine := make(map[wire.PeerID]time.Duration, len(local))
	for _, e := range local {
		if e.RTT > 0 {
			mine[e.Peer] = e.RTT
		}
	}
	var currentSum time.Duration
	for _, e := range remote {
		rtt, ok := mine[e.Peer]
		if !ok || e.RTT <= 0 {
			continue
		}
		total := rtt + e.RTT
		if e.Peer == current {
			currentSum = total
		}
		if best == "" || total < sum {
			best, sum = e.Peer, total
		}
	}
	if best == "" || best == current {
		return best, sum, false
	}
	if currentSum == 0 {
		return best, sum, true
	}
	return best, sum, sum+5*time.Millisecond <= currentSum && sum*5 <= currentSum*4
}
