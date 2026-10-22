package cluster

import (
	"slices"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Repair authority (docs/cluster.md "Sync, handoff and repair", 0048): a member holding
// an admin key with repair on marks storage members `unavailable` after DownAfter without
// a heartbeat (or when Failed reports repeated audit failures) and `active` again after
// UpAfter of resumed heartbeats. The epoch change makes the next-ranked members acquire
// the blobs; without an online authority nothing is re-placed and status says so.

// Repair defaults.
const (
	DefaultDownAfter = 15 * time.Minute
	DefaultUpAfter   = 10 * time.Minute
)

// RepairConfig turns a service into a repair authority (while it holds an admin key).
type RepairConfig struct {
	DownAfter, UpAfter time.Duration
	// Failed reports repeated audit failures of a member; nil: none.
	Failed func(wire.PeerID) bool
}

// repairConfig returns the active repair configuration: nil unless configured and this
// node holds an admin key.
func (s *Service) repairConfig() *RepairConfig {
	if s.cfg.Repair == nil {
		return nil
	}
	rc := s.cfg.Repair()
	if rc == nil || !s.c.HoldsKey(RoleAdmin) {
		return nil
	}
	return rc
}

// RepairAuthorities reports whether this node is a repair authority and which connected
// members announced one in a heartbeat received within three intervals.
func (s *Service) RepairAuthorities() (self bool, peers []wire.PeerID) {
	self = s.repairConfig() != nil
	now := s.c.Now()
	v := s.c.View()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.peers {
		if p.Heartbeat != nil && p.Heartbeat.Repair && now.Sub(p.Received) <= 3*s.cfg.Interval && v.IsMember(id) && s.Connected(id) {
			peers = append(peers, id)
		}
	}
	slices.Sort(peers)
	return self, peers
}

// LastHeard is when this node last received a heartbeat from a member (zero: never since
// the service started).
func (s *Service) LastHeard(id wire.PeerID) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[id]; p != nil && p.Heartbeat != nil {
		return p.Received
	}
	return time.Time{}
}

// noteHeartbeatLocked tracks continuous heartbeats for up_after.
func (s *Service) noteHeartbeatLocked(id wire.PeerID, prev, now time.Time) {
	if prev.IsZero() || now.Sub(prev) > 3*s.cfg.Interval {
		s.upSince[id] = now
	}
}

// repairPass runs one round of the repair authority.
func (s *Service) repairPass() {
	rc := s.repairConfig()
	if rc == nil {
		return
	}
	now := s.c.Now()
	v := s.c.View()
	for id, m := range v.Members {
		if m.Deleted || m.Value == nil || id == s.self || !slices.Contains(m.Value.Roles, RoleStorage) {
			continue
		}
		s.mu.Lock()
		last, up := s.started, s.upSince[id]
		if p := s.peers[id]; p != nil && p.Heartbeat != nil {
			last = p.Received
		}
		s.mu.Unlock()
		failed := rc.Failed != nil && rc.Failed(id)
		rec := *m.Value
		switch rec.State {
		case StateActive:
			if now.Sub(last) <= rc.DownAfter && !failed {
				continue
			}
			rec.State, rec.Auto = StateUnavailable, true
			s.log.Warn("repair: marking member unavailable", "peer", id, "silent", now.Sub(last).Round(time.Second), "audits_failed", failed)
		case StateUnavailable:
			if !rec.Auto || up.IsZero() || now.Sub(last) > 3*s.cfg.Interval || now.Sub(up) < rc.UpAfter || failed {
				continue
			}
			rec.State, rec.Auto = StateActive, false
			s.log.Info("repair: member is back", "peer", id)
		default:
			continue
		}
		if _, err := s.c.Write(Op{Collection: NSMember, RKey: id.String(), Value: &rec}); err != nil {
			s.log.Warn("repair: cannot write member record", "peer", id, "err", err)
		}
	}
}
