package collection

import (
	"strconv"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func parsePeerID(s string) (string, error) {
	id, err := wire.ParsePeerID(s)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// base36 renders a stored (base58) PeerId in the API's base36 form.
func base36(s string) string {
	id, err := wire.ParsePeerID(s)
	if err != nil {
		return s
	}
	return id.Base36()
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// indexStatus summarises the holdings index: per-member holdings and cursors, unheld
// items and the copy-count histogram.
func (m *Manager) indexStatus(name string, members []wire.PeerID) (*apiv0.CollectionIndex, error) {
	fresh := m.freshness()
	out := &apiv0.CollectionIndex{Members: []apiv0.CollectionMember{}, Copies: map[string]int{}}
	counts, _, err := m.copies(name, fresh, nil)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.Query(`SELECT path FROM items WHERE ds = ? AND deleted = 0`, name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			c := counts[p]
			out.Copies[strconv.Itoa(c)]++
			if c == 0 {
				out.Unheld++
			}
		}
	}
	rows.Close()
	marks := map[string]int64{}
	if m.cfg.Service != nil {
		if svc := m.cfg.Service(); svc != nil {
			for id, p := range svc.Peers() {
				if p.Heartbeat == nil {
					continue
				}
				for _, hm := range p.Heartbeat.Holdings {
					if hm.Dataset == name {
						marks[id.String()] = hm.Seq
					}
				}
			}
		}
	}
	_, marks[m.cfg.Self.String()] = m.LogMark(name)
	for _, id := range members {
		peer := id.String()
		mi := apiv0.CollectionMember{Peer: id.Base36(), Fresh: fresh[peer]}
		_ = m.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM idx WHERE ds = ? AND peer = ? AND state = ?`, name, peer, StateComplete).
			Scan(&mi.Items, &mi.Bytes)
		if c, ok := m.loadCursor(name, peer); ok {
			mi.Epoch, mi.Seq = c.epoch, c.seq
		}
		if mk, ok := marks[peer]; ok && mk > mi.Seq {
			mi.Behind = true
		}
		out.Members = append(out.Members, mi)
	}
	return out, nil
}
