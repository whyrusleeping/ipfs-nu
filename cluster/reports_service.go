package cluster

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// maxDeltaBytes bounds the reports part of a heartbeat; a larger delta makes receivers
// pull the full set instead.
const maxDeltaBytes = 48 << 10

// MaxReports bounds a member's report set (one per dataset) and MaxReportsBody its
// encoding on the `reports` op.
const (
	MaxReports     = 100_000
	MaxReportsBody = 8 << 20
)

// nextReportSeq is max(last+1, now in µs), so report sequences keep increasing across
// restarts (E4: a newer seq replaces an older report).
func (s *Service) nextReportSeq() int64 {
	n := max(s.ownSeq+1, s.c.Now().UnixMicro())
	s.ownSeq = n
	return n
}

// reportDelta numbers this node's changed reports and returns the heartbeat delta
// against the previous heartbeat.
func (s *Service) reportDelta(cur []Report) *ReportDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range cur {
		seen[r.Name] = true
		if old, ok := s.own[r.Name]; ok && old.same(r) {
			continue
		}
		r.Seq = s.nextReportSeq()
		s.own[r.Name] = r
	}
	var gone []string
	for name := range s.own {
		if !seen[name] {
			gone = append(gone, name)
			delete(s.own, name)
		}
	}
	if len(gone) > 0 {
		s.nextReportSeq()
	}
	slices.Sort(gone)
	d := &ReportDelta{Base: s.sentSeq, Seq: s.ownSeq, Gone: gone}
	if !s.sentOnce {
		d.Base, d.Gone = 0, nil
	}
	for _, r := range s.own {
		if r.Seq > d.Base {
			d.List = append(d.List, r)
		}
	}
	slices.SortFunc(d.List, func(a, b Report) int { return cmpStr(a.Name, b.Name) })
	if b, err := drisl.Marshal(d); err != nil || len(b) > maxDeltaBytes {
		d.Base, d.List, d.Gone = -1, nil, nil
	}
	s.sentSeq, s.sentOnce = s.ownSeq, true
	return d
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// KickHeartbeat sends a heartbeat now (after this node's reports changed), instead of
// at the next interval.
func (s *Service) KickHeartbeat() {
	select {
	case s.hbKick <- struct{}{}:
	default:
	}
}

// ownSet returns this node's report set as last numbered.
func (s *Service) ownSet() ReportSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := ReportSet{Seq: s.ownSeq, Reports: []Report{}}
	for _, r := range s.own {
		set.Reports = append(set.Reports, r)
	}
	slices.SortFunc(set.Reports, func(a, b Report) int { return cmpStr(a.Name, b.Name) })
	return set
}

// applyReports applies a verified heartbeat's delta from peer (s.mu held). It returns
// true when the peer's full set must be pulled.
func (s *Service) applyReportsLocked(p *PeerStatus, d *ReportDelta, now time.Time) bool {
	switch {
	case d.Base == 0:
		p.Reports = map[string]Report{}
	case d.Base < 0 || d.Base != p.RSeq || p.Reports == nil:
		return true
	}
	if len(p.Reports)+len(d.List) > MaxReports {
		return true
	}
	for _, r := range d.List {
		if old, ok := p.Reports[r.Name]; !ok || r.Seq > old.Seq {
			p.Reports[r.Name] = r
		}
	}
	for _, name := range d.Gone {
		delete(p.Reports, name)
	}
	p.RSeq, p.ReportsAt = d.Seq, now
	s.evidenceChangedLocked()
	return false
}

func (s *Service) evidenceChangedLocked() {
	close(s.evidence)
	s.evidence = make(chan struct{})
}

// pullReports fetches peer's full report set.
func (s *Service) pullReports(peer wire.PeerID) {
	s.mu.Lock()
	p := s.peers[peer]
	if p == nil || p.pulling {
		s.mu.Unlock()
		return
	}
	p.pulling = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		p.pulling = false
		s.mu.Unlock()
	}()
	var set ReportSet
	_, err := call(s.ctx, s.h, peer, &request{Op: OpReports, Cluster: s.c.ID}, func(r io.Reader) error {
		b, err := wire.ReadMessage(bufio.NewReader(r), MaxReportsBody)
		if err != nil {
			return err
		}
		return (drisl.DecOptions{MaxSize: MaxReportsBody, MaxDepth: 8}).Unmarshal(b, &set)
	})
	if err == nil && len(set.Reports) > MaxReports {
		err = errors.New("cluster: too many reports")
	}
	if err != nil {
		s.log.Debug("reports pull failed", "peer", peer, "err", err)
		return
	}
	now := s.c.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if set.Seq < p.RSeq {
		return // a heartbeat already brought something newer
	}
	p.Reports = map[string]Report{}
	for _, r := range set.Reports {
		p.Reports[r.Name] = r
	}
	p.RSeq, p.ReportsAt = set.Seq, now
	s.evidenceChangedLocked()
}

// serveReports answers a `reports` request with this node's full set.
func (s *Service) serveReports() ([]byte, error) {
	body, err := drisl.Marshal(s.ownSet())
	if err != nil {
		return nil, err
	}
	if len(body) > MaxReportsBody {
		return nil, errors.New("report set too large")
	}
	var framed bytes.Buffer
	if err := wire.WriteMessage(&framed, body, MaxReportsBody); err != nil {
		return nil, err
	}
	return framed.Bytes(), nil
}

// Evidence returns the latest received report of every member for dataset name, with
// the time each was last confirmed (E4: callers apply evidence.fresh).
func (s *Service) Evidence(name string) map[wire.PeerID]Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[wire.PeerID]Evidence{}
	for id, p := range s.peers {
		if r, ok := p.Reports[name]; ok {
			out[id] = Evidence{Report: r, Received: p.ReportsAt}
		}
	}
	return out
}

// WaitEvidence blocks until received reports change or ctx ends.
func (s *Service) WaitEvidence(ctx context.Context) {
	s.mu.Lock()
	ch := s.evidence
	s.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-ch:
	}
}

// EvidenceChanged returns a channel closed at the next change of received reports.
func (s *Service) EvidenceChanged() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evidence
}
