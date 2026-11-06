package swarm

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Opt-in field NAT telemetry (D11c §6, swarm.nat_telemetry). One record per
// disco probe session: the first direct path, or "relayed" when none was
// selected within telemetryWindow. Sessions ending earlier are not recorded.
// Records go only to a local NDJSON file; nothing is sent anywhere.
const (
	telemetryWindow   = time.Minute
	telemetryMaxBytes = 1 << 20
)

// natTelemetryRecord is the complete record: no content, PeerIDs, addresses or
// timestamps. It is the natlab.Outcome shape p2p/natlab/cmd/nattable ingests.
type natTelemetryRecord struct {
	Scenario     string        `json:"scenario"` // always "field"
	From         string        `json:"from"`     // this node's NAT profile label
	To           string        `json:"to"`       // the peer's published profile label
	Strategy     string        `json:"strategy"` // lan, ipv6, mapped, probe or hard-nat
	Outcome      string        `json:"outcome"`  // direct or relayed
	TimeToDirect time.Duration `json:"time_to_direct_ns,omitempty"`
	Version      string        `json:"nu"`
}

type telemetrySession struct {
	timer *time.Timer
	done  bool
}

type telemetryResult struct {
	peer     wire.PeerID
	strategy string
	direct   bool
	ttd      time.Duration
}

type natTelemetry struct {
	path    string
	status  func(wire.PeerID) (disco.Status, bool)
	local   func() (netcheck.Profile, bool)
	remote  func(context.Context, wire.PeerID) (netcheck.Profile, bool)
	queue   chan telemetryResult
	written atomic.Int64
	mu      sync.Mutex // leaf lock: observe runs under the disco lock
	open    map[wire.PeerID]*telemetrySession
}

func newNATTelemetry(path string) *natTelemetry {
	return &natTelemetry{path: path, queue: make(chan telemetryResult, 64), open: make(map[wire.PeerID]*telemetrySession)}
}

// observe follows disco events; it must stay quick and never call disco.
func (t *natTelemetry) observe(e disco.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.open[e.Peer]
	switch e.Kind {
	case disco.SessionStarted:
		if s != nil {
			s.timer.Stop()
		}
		s = &telemetrySession{}
		s.timer = time.AfterFunc(telemetryWindow, func() { t.expire(e.Peer, s) })
		t.open[e.Peer] = s
	case disco.SessionEnded:
		if s != nil {
			s.timer.Stop()
			delete(t.open, e.Peer)
		}
	case disco.PathDirect:
		if s == nil || s.done {
			return
		}
		s.done = true
		s.timer.Stop()
		strategy := "probe"
		switch {
		case e.Hard:
			strategy = "hard-nat"
		case e.Via == disco.LAN:
			strategy = "lan"
		case e.Via == disco.IPv6:
			strategy = "ipv6"
		case e.Via == disco.Mapped:
			strategy = "mapped"
		}
		t.enqueue(telemetryResult{peer: e.Peer, strategy: strategy, direct: true, ttd: max(e.Elapsed, time.Nanosecond)})
	}
}

func (t *natTelemetry) expire(peer wire.PeerID, s *telemetrySession) {
	t.mu.Lock()
	current := t.open[peer] == s && !s.done
	s.done = true
	t.mu.Unlock()
	if !current {
		return
	}
	st, ok := t.status(peer)
	if !ok {
		return
	}
	strategy := "probe"
	if st.HardAttempts > 0 {
		strategy = "hard-nat"
	}
	t.mu.Lock()
	t.enqueue(telemetryResult{peer: peer, strategy: strategy})
	t.mu.Unlock()
}

func (t *natTelemetry) enqueue(r telemetryResult) {
	select {
	case t.queue <- r:
	default: // a full queue drops the record rather than blocking disco
	}
}

// run labels queued results (fetching the peer's published profile) and appends
// them to the export file until ctx ends.
func (t *natTelemetry) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-t.queue:
			rec := natTelemetryRecord{Scenario: "field", From: "unknown", To: "unknown", Strategy: r.strategy, Outcome: "relayed", Version: nu.Version}
			if r.direct {
				rec.Outcome, rec.TimeToDirect = "direct", r.ttd
			}
			if p, ok := t.local(); ok {
				rec.From = profileLabel(p)
			}
			fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if p, ok := t.remote(fctx, r.peer); ok {
				rec.To = profileLabel(p)
			}
			cancel()
			if t.append(rec) == nil {
				t.written.Add(1)
			}
		}
	}
}

func (t *natTelemetry) append(rec natTelemetryRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil || fi.Size()+int64(len(b)) >= telemetryMaxBytes {
		f.Close()
		return os.ErrInvalid // full: the operator collects and removes the file
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// profileLabel names an IPv4 profile in the per-pair table's vocabulary
// (public, EIM/EIF, APDM-random/APDF, mapped (UPnP), ...).
func profileLabel(p netcheck.Profile) string {
	switch {
	case p.UDP == netcheck.UDPBlocked:
		return "UDP-blocked"
	case p.UDP != netcheck.UDPOK || p.Mapping == netcheck.MappingUnknown:
		return "unknown"
	case p.PortMap&netcheck.PortMapUPnP != 0:
		return "mapped (UPnP)"
	case p.PortMap&netcheck.PortMapPCP != 0:
		return "mapped (PCP)"
	case p.PortMap&netcheck.PortMapNATPMP != 0:
		return "mapped (NAT-PMP)"
	}
	filtering, allocation := "?", "?"
	if p.Filtering <= netcheck.FilterAddressPortDependent {
		filtering = [...]string{"?", "EIF", "ADF", "APDF"}[p.Filtering]
	}
	if p.Allocation <= netcheck.AllocRandom {
		allocation = [...]string{"?", "preserving", "seq", "random"}[p.Allocation]
	}
	switch p.Mapping {
	case netcheck.MappingNone:
		if p.Filtering == netcheck.FilterEndpointIndependent || p.Filtering == netcheck.FilterUnknown {
			return "public"
		}
		return "firewall/" + filtering
	case netcheck.MappingEndpointIndependent:
		return "EIM/" + filtering
	case netcheck.MappingAddressDependent:
		return "ADM-" + allocation + "/" + filtering
	case netcheck.MappingAddressPortDependent:
		return "APDM-" + allocation + "/" + filtering
	}
	return "EDM-" + allocation + "/" + filtering
}
