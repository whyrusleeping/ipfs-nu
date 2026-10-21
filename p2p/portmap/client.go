// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/portmapper/{portmapper,pcp,upnp}.go;
// see NOTICE. Nu replaces async/control-plane coupling with a bounded lease owner.
package portmap

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

var ErrProtocol = errors.New("portmap: invalid or unsuccessful protocol response")

type Protocol string

const (
	PCP  Protocol = "pcp"
	PMP  Protocol = "nat-pmp"
	UPnP Protocol = "upnp"
)

// Config describes one bound UDP socket and its explicitly selected gateway.
// The caller must replace the Client when the gateway or local address changes.
// No mapping is an address verification: expose it as a tentative candidate.
type Config struct {
	Gateway netip.Addr
	Local   netip.AddrPort
	// Remote requests PCP PEER rather than MAP; other protocols are not used.
	// IPv6 Local uses PCP MAP/PEER to request a firewall pinhole.
	Remote       netip.AddrPort
	Protocols    []Protocol    // nil: PCP, NAT-PMP, UPnP
	Lifetime     time.Duration // default 2h, range 1s..24h
	Timeout      time.Duration // per protocol operation, default 2s
	PollInterval time.Duration // epoch probing/retry, default 30s
	ServerPort   uint16        // PCP/NAT-PMP, default 5351
	SSDPPort     uint16        // UPnP gateway discovery, default 1900
}

// Mapping is immutable provenance for a candidate, not a promise of reachability.
// Remote is nonzero for a peer-specific mapping and MUST constrain its use.
type Mapping struct {
	Protocol                   Protocol
	Internal, External, Remote netip.AddrPort
	RenewAfter, Expires        time.Time
	Epoch                      uint32
	Reboot                     bool // epoch discontinuity detected while obtaining this lease
}

// Client owns one lease. Methods are concurrency-safe; run at most one Run loop.
// Shutdown: cancel/join Run, then Release with a fresh bounded context.
type Client struct {
	cfg     Config
	op      chan struct{} // context-aware serialization of network operations
	mu      sync.Mutex
	mapping Mapping
	nonce   [12]byte
	epoch   uint32
	epochAt time.Time
	service *upnpService
	changes chan struct{}
	wake    chan struct{} // private renewal-loop wakeup; never steals subscriber signals
}

func New(cfg Config) (*Client, error) {
	if !cfg.Gateway.IsValid() || cfg.Gateway.IsUnspecified() || cfg.Gateway.IsMulticast() || !cfg.Local.IsValid() || cfg.Local.Port() == 0 || cfg.Local.Addr().IsUnspecified() || cfg.Local.Addr().IsMulticast() {
		return nil, errors.New("portmap: gateway and bound local UDP address required")
	}
	if cfg.Remote.IsValid() && (cfg.Remote.Port() == 0 || cfg.Remote.Addr().IsUnspecified() || cfg.Remote.Addr().IsMulticast() || cfg.Remote.Addr().Is4() != cfg.Local.Addr().Is4()) {
		return nil, errors.New("portmap: invalid remote peer")
	}
	if cfg.Lifetime == 0 {
		cfg.Lifetime = 2 * time.Hour
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Lifetime < time.Second || cfg.Lifetime > 24*time.Hour || cfg.Timeout <= 0 || cfg.PollInterval <= 0 {
		return nil, errors.New("portmap: invalid duration")
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 5351
	}
	if cfg.SSDPPort == 0 {
		cfg.SSDPPort = 1900
	}
	if cfg.Protocols == nil {
		cfg.Protocols = []Protocol{PCP, PMP, UPnP}
	}
	cfg.Protocols = append([]Protocol(nil), cfg.Protocols...)
	for _, p := range cfg.Protocols {
		if p != PCP && p != PMP && p != UPnP {
			return nil, errors.New("portmap: unknown protocol")
		}
	}
	c := &Client{cfg: cfg, op: make(chan struct{}, 1), changes: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	if _, err := rand.Read(c.nonce[:]); err != nil {
		return nil, err
	}
	return c, nil
}

// Changes coalesces candidate changes/invalidations. One consumer should fan out
// notifications and call Candidate; the channel is not closed by Release.
func (c *Client) Changes() <-chan struct{} { return c.changes }
func (c *Client) Candidate() (Mapping, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mapping, c.mapping.External.IsValid() && time.Now().Before(c.mapping.Expires)
}
func (c *Client) publish(m Mapping) {
	c.mu.Lock()
	c.mapping = m
	c.mu.Unlock()
	select {
	case c.changes <- struct{}{}:
	default:
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Refresh requests a fresh lease, also used as the D12 mapping-refresh hook.
func (c *Client) Refresh(ctx context.Context) (Mapping, error) {
	if err := c.lock(ctx); err != nil {
		return Mapping{}, err
	}
	defer c.unlock()
	return c.refresh(ctx, false)
}
func (c *Client) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.op <- struct{}{}:
		return nil
	}
}
func (c *Client) unlock() { <-c.op }
func (c *Client) refresh(ctx context.Context, reboot bool) (Mapping, error) {
	old, _ := c.Candidate()
	var errs []error
	for _, p := range c.cfg.Protocols {
		if (c.cfg.Remote.IsValid() || c.cfg.Local.Addr().Is6()) && p != PCP {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		start := time.Now()
		m, err := c.mapWith(attempt, p, old, uint32(c.cfg.Lifetime/time.Second))
		cancel()
		if err != nil {
			if p == UPnP {
				c.service = nil
			} // rediscover after router/service restarts
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if !m.External.IsValid() || m.External.Port() == 0 || m.External.Addr().IsUnspecified() || m.External.Addr().IsMulticast() || !m.Expires.After(time.Now()) {
			errs = append(errs, ErrProtocol)
			continue
		}
		m.Protocol = p
		m.Internal = c.cfg.Local
		m.Remote = c.cfg.Remote
		// A successful MAP/AddPortMapping already restores our one lease after reboot.
		m.Reboot = reboot || (old.Protocol == p && c.epochChanged(m.Epoch, start))
		c.epoch, c.epochAt = m.Epoch, start
		c.publish(m)
		return m, nil
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("portmap: no eligible mapping protocol"))
	}
	return Mapping{}, errors.Join(errs...)
}
func lease(external netip.AddrPort, epoch, seconds uint32, start time.Time) Mapping {
	// Router leases keep aging while the laptop sleeps; Linux monotonic time
	// does not. Wall deadlines conservatively expire across suspend/clock jumps.
	start = start.Round(0)
	d := time.Duration(seconds) * time.Second
	return Mapping{External: external, Epoch: epoch, RenewAfter: start.Add(d / 2), Expires: start.Add(d)}
}
func (c *Client) mapWith(ctx context.Context, p Protocol, old Mapping, seconds uint32) (Mapping, error) {
	start := time.Now()
	external := netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	if c.cfg.Local.Addr().Is6() {
		external = netip.AddrPortFrom(netip.IPv6Unspecified(), 0)
	}
	if old.Protocol == p && seconds != 0 {
		external = old.External
	}
	switch p {
	case PCP:
		req := pcpPacket(pcpRequest{Local: c.cfg.Local, External: external, Remote: c.cfg.Remote, Nonce: c.nonce, Lifetime: seconds})
		b, err := c.exchange(ctx, c.cfg.ServerPort, req, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		r, err := parsePCP(b, req, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		return lease(r.External, r.Epoch, r.Lifetime, start), nil
	case PMP:
		b, err := c.exchange(ctx, c.cfg.ServerPort, []byte{0, 0}, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		pub, err := parsePMP(b, 0, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		req := pmpPacket(c.cfg.Local.Port(), external.Port(), seconds)
		b, err = c.exchange(ctx, c.cfg.ServerPort, req, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		r, err := parsePMP(b, 1, maxPacket)
		if err != nil {
			return Mapping{}, err
		}
		if r.Local != c.cfg.Local.Port() {
			return Mapping{}, ErrProtocol
		}
		return lease(netip.AddrPortFrom(pub.Public, r.External), r.Epoch, r.Lifetime, start), nil
	case UPnP:
		return c.mapUPnP(ctx, old, seconds, start)
	}
	return Mapping{}, ErrProtocol
}

// Check probes the router epoch even before renewal is due. A reboot invalidates
// the candidate before remapping. Epoch lag tolerates clock quantization/drift.
func (c *Client) Check(ctx context.Context) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	m, ok := c.Candidate()
	if !ok || !time.Now().Before(m.RenewAfter) {
		_, err := c.refresh(ctx, false)
		return err
	}
	attempt, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	epoch, err := c.probeEpoch(attempt, m.Protocol)
	if err != nil {
		if m.Protocol == UPnP {
			c.service = nil
		}
		return err
	}
	if c.epochChanged(epoch, time.Now()) {
		c.publish(Mapping{})
		c.epochAt = time.Time{}
		c.service = nil
		if _, err = rand.Read(c.nonce[:]); err != nil {
			return err
		}
		_, err = c.refresh(ctx, true)
		return err
	}
	c.epoch, c.epochAt = epoch, time.Now()
	return nil
}
func (c *Client) epochChanged(epoch uint32, now time.Time) bool {
	if c.epochAt.IsZero() {
		return false
	}
	elapsed := max(0, now.Round(0).Sub(c.epochAt.Round(0)).Seconds())
	return epoch < c.epoch || float64(epoch)+2 < float64(c.epoch)+elapsed*0.875
}
func (c *Client) probeEpoch(ctx context.Context, p Protocol) (uint32, error) {
	switch p {
	case PCP:
		req := pcpAnnounce(c.cfg.Local.Addr())
		b, err := c.exchange(ctx, c.cfg.ServerPort, req, maxPacket)
		if err != nil {
			return 0, err
		}
		r, err := parsePCP(b, req, maxPacket)
		return r.Epoch, err
	case PMP:
		b, err := c.exchange(ctx, c.cfg.ServerPort, []byte{0, 0}, maxPacket)
		if err != nil {
			return 0, err
		}
		r, err := parsePMP(b, 0, maxPacket)
		return r.Epoch, err
	case UPnP:
		return c.upnpEpoch(ctx)
	}
	return 0, ErrProtocol
}

// Run maps immediately, probes epochs, retries transient failures, and renews at
// half-life. Errors are returned by explicit Refresh/Check; Run retries until
// cancellation. A failed renewal never extends the old candidate's expiry.
func (c *Client) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.Check(ctx)
		wait := c.cfg.PollInterval
		if err != nil {
			wait = min(wait, time.Second)
		}
		if m, ok := c.Candidate(); ok {
			if d := time.Until(m.RenewAfter); d > 0 && d < wait {
				wait = d
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case <-c.wake:
			timer.Stop()
		}
	}
}

// Release removes the candidate immediately and makes a best-effort bounded
// lease deletion. It does not stop Run; cancel/join that loop first.
func (c *Client) Release(ctx context.Context) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	m, _ := c.Candidate()
	c.publish(Mapping{})
	c.epochAt = time.Time{}
	if !m.External.IsValid() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	_, err := c.mapWith(ctx, m.Protocol, m, 0)
	return err
}

func (c *Client) exchange(ctx context.Context, port uint16, req []byte, maxBytes int) ([]byte, error) {
	d := net.Dialer{LocalAddr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.cfg.Local.Addr(), 0))}
	conn, err := d.DialContext(ctx, "udp", netip.AddrPortFrom(c.cfg.Gateway, port).String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.cfg.Timeout)
	}
	b := make([]byte, maxBytes+1)
	for retry := 250 * time.Millisecond; ; retry = min(2*retry, time.Second) {
		if err = conn.SetDeadline(minTime(deadline, time.Now().Add(retry))); err != nil {
			return nil, err
		}
		if _, err = conn.Write(req); err != nil {
			return nil, err
		}
		n, e := conn.Read(b)
		if e == nil {
			if n > maxBytes {
				return nil, ErrProtocol
			}
			return b[:n], nil
		}
		var ne net.Error
		if !errors.As(e, &ne) || !ne.Timeout() || !time.Now().Before(deadline) || ctx.Err() != nil {
			return nil, e
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
