package swarm

import (
	"context"
	"errors"

	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/portmap"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
)

// 0018 NAT profiling seam (public mode only; runNetwork is never reached in
// private/isolated mode). Observers are the connected configured relays that
// serve /nu/netcheck/1. The profile is served to peers on /nu/natprofile/1 and
// exposed through NATProfile for status/doctor.
func (c *connectivity) startNetcheck(pipeline *netmon.Pipeline) error {
	n := c.network
	cfg := netcheck.MonitorConfig{Observers: c.netcheckObservers}
	if n.mapper != nil {
		cfg.PortMap = func(context.Context) netcheck.PortMaps {
			m, ok := n.mapper.Candidate()
			if !ok {
				return 0
			}
			switch m.Protocol {
			case portmap.PCP:
				return netcheck.PortMapPCP
			case portmap.PMP:
				return netcheck.PortMapNATPMP
			case portmap.UPnP:
				return netcheck.PortMapUPnP
			}
			return 0
		}
	}
	mon, err := netcheck.StartMonitor(c.ctx, cfg)
	if err != nil {
		return err
	}
	if !c.spawn(func() { <-c.ctx.Done(); mon.Close() }) {
		mon.Close()
		return context.Canceled
	}
	// Startup usually precedes relay connections: until a first report exists,
	// re-run as soon as another configured relay connects (not only on retry).
	c.spawn(func() {
		changes, stop := c.s.host.WatchChanges()
		defer stop()
		seen := 0
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-changes:
			}
			if _, ok, _ := mon.Current(); ok {
				return
			}
			if n := c.connectedRelays(); n > seen {
				mon.Trigger(false)
				seen = n
			}
		}
	})
	if _, err := pipeline.Register(netmon.RestartProbing, mon.Hook()); err != nil {
		return err
	}
	if err := netcheck.ServeProfiles(c.s.host, mon.Profile); err != nil {
		return err
	}
	n.mu.Lock()
	n.natProfile = mon
	n.mu.Unlock()
	return nil
}

func (c *connectivity) connectedRelays() int {
	n := 0
	for _, relay := range c.s.cfg.Relays {
		if id, err := circuit.RelayPeer(relay); err == nil {
			if _, ok := c.s.host.Connection(id); ok {
				n++
			}
		}
	}
	return n
}

func (c *connectivity) netcheckObservers(ctx context.Context) ([]netcheck.Observer, error) {
	var out []netcheck.Observer
	var errs []error
	for _, relay := range c.s.cfg.Relays {
		id, err := circuit.RelayPeer(relay)
		if err != nil {
			continue
		}
		if _, ok := c.s.host.Connection(id); !ok {
			continue
		}
		d, err := netcheck.Fetch(ctx, c.s.host, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, netcheck.Observers(id.String(), d)...)
	}
	if len(out) == 0 {
		return nil, errors.Join(append([]error{netcheck.ErrNoObservers}, errs...)...)
	}
	return out, nil
}

// NATProfile returns the latest local NAT report (ok=false before the first
// measurement, or in private/isolated mode) and the last profiling error.
func (s *Swarm) NATProfile() (netcheck.Report, bool, error) {
	c := s.connectivity
	if c == nil {
		return netcheck.Report{}, false, nil
	}
	c.mu.Lock()
	n := c.network
	c.mu.Unlock()
	if n == nil {
		return netcheck.Report{}, false, nil
	}
	n.mu.Lock()
	mon := n.natProfile
	n.mu.Unlock()
	if mon == nil {
		return netcheck.Report{}, false, nil
	}
	return mon.Current()
}
