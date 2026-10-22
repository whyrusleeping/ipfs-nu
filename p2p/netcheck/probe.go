package netcheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Observer is one /nu/netcheck/1 server's endpoints in one address family.
type Observer struct {
	Name string `json:"name,omitempty"` // diagnostics only (e.g. relay PeerID)
	EndpointSet
}

// Observers flattens descriptors into per-family observers.
func Observers(name string, d Descriptor) []Observer {
	out := make([]Observer, 0, len(d.Sets))
	for _, s := range d.Sets {
		out = append(out, Observer{Name: name, EndpointSet: s})
	}
	return out
}

// Config tunes a profile run. Zero values select the documented defaults.
type Config struct {
	// ListenPacket opens each fresh probe socket ("udp4"/"udp6"); default
	// net.ListenPacket(network, ":0"). Sockets are opened in a deterministic order.
	ListenPacket func(network string) (net.PacketConn, error)
	Timeout      time.Duration // 1s total wait per exchange
	Retransmit   time.Duration // 250ms between request copies
	// AllocationSamples fresh sockets map toward one observer back to back (5).
	AllocationSamples int
	// Mapping lifetime ladder: coarse doubling from LifetimeMin (5s) to
	// LifetimeMax (240s), then three fine rungs inside the failing octave.
	LifetimeMin, LifetimeMax time.Duration
	LifetimeTimeout          time.Duration // 400ms per liveness check (retransmit every 100ms)
	MaxDelta, MaxJitter      int           // sequential if |median delta| <= 256 and jitter <= 64
	MaxObservers             int           // per family (4)
	// PortMap reports available router mapping protocols (owner-supplied).
	PortMap func(context.Context) PortMaps
}

func (c Config) withDefaults() Config {
	if c.ListenPacket == nil {
		c.ListenPacket = func(network string) (net.PacketConn, error) { return net.ListenPacket(network, ":0") }
	}
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.Timeout, time.Second)
	set(&c.Retransmit, 250*time.Millisecond)
	set(&c.LifetimeMin, 5*time.Second)
	set(&c.LifetimeMax, 240*time.Second)
	set(&c.LifetimeTimeout, 400*time.Millisecond)
	if c.LifetimeMax < c.LifetimeMin {
		c.LifetimeMax = c.LifetimeMin
	}
	if c.AllocationSamples < 3 {
		c.AllocationSamples = 5
	}
	if c.MaxDelta <= 0 {
		c.MaxDelta = 256
	}
	if c.MaxJitter <= 0 {
		c.MaxJitter = 64
	}
	if c.MaxObservers <= 0 {
		c.MaxObservers = 4
	}
	return c
}

// Observation is one observer endpoint's view of the mapping socket.
type Observation struct {
	Observer netip.AddrPort `json:"observer"`
	Mapped   netip.AddrPort `json:"mapped"`
}

// Sample is one fresh socket's first mapping toward the allocation observer.
type Sample struct {
	Local  netip.AddrPort `json:"local"`
	Mapped netip.AddrPort `json:"mapped"`
}

// Evidence records the raw filtering probes: "received", "missing" or "untested".
type Evidence struct {
	AltPort string `json:"alt_port"`
	AltIP   string `json:"alt_ip"`
	Via     string `json:"via,omitempty"` // "alt-ip" or the cross-observer endpoint
	Hairpin string `json:"hairpin"`
}

// FamilyReport is local diagnostic detail (contains addresses; never exchanged).
type FamilyReport struct {
	Local        netip.AddrPort `json:"local,omitzero"`
	Observations []Observation  `json:"observations,omitempty"`
	Samples      []Sample       `json:"samples,omitempty"`
	Filtering    Evidence       `json:"filtering"`
	Error        string         `json:"error,omitempty"`
}

// Lifetime is the mapping-lifetime ladder outcome: alive at Low, gone at High.
type Lifetime struct {
	Low      time.Duration `json:"low_ns"`
	High     time.Duration `json:"high_ns,omitempty"` // zero: never expired up to Low
	Estimate time.Duration `json:"estimate_ns"`
	Bound    Bound         `json:"bound"`
	Probes   int           `json:"probes"`
}

// Report is the local result: exchange Profile, show the rest in `nu doctor`.
type Report struct {
	Profile  Profile       `json:"profile"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration_ns"`
	IPv4     FamilyReport  `json:"ipv4"`
	IPv6     FamilyReport  `json:"ipv6"`
	Lifetime *Lifetime     `json:"lifetime,omitempty"`
	// Primary is the IPv4 observer used for allocation/lifetime probes.
	Primary *Observer `json:"primary,omitempty"`
}

var ErrNoObservers = errors.New("netcheck: no usable observers")

// Run performs Probe and, when IPv4 UDP works, the mapping-lifetime ladder.
func Run(ctx context.Context, cfg Config, observers []Observer) (Report, error) {
	r, err := Probe(ctx, cfg, observers)
	if err != nil || r.Profile.UDP != UDPOK {
		return r, err
	}
	l, err := MeasureLifetime(ctx, cfg, observers)
	if err != nil {
		return r, err
	}
	r.SetLifetime(l)
	r.Duration = time.Since(r.Started)
	return r, nil
}

// SetLifetime merges a ladder result into the report and its profile.
func (r *Report) SetLifetime(l Lifetime) {
	r.Lifetime = &l
	if r.Profile.UDP != UDPOK || l.Estimate <= 0 {
		return
	}
	r.Profile.Lifetime = uint16(min(math.MaxUint16, max(1, math.Round(l.Estimate.Seconds()))))
	r.Profile.LifetimeBound = l.Bound
}

// Probe classifies mapping, allocation, filtering and hairpin (IPv4) and IPv6
// availability/mapping/filtering. It takes roughly 2-3 x Timeout.
func Probe(ctx context.Context, cfg Config, observers []Observer) (Report, error) {
	cfg = cfg.withDefaults()
	r := Report{Started: time.Now()}
	var v4, v6 []Observer
	for _, o := range observers {
		if o.Validate() != nil {
			continue
		}
		if o.Primary.Addr().Is4() {
			v4 = append(v4, o)
		} else {
			v6 = append(v6, o)
		}
	}
	if len(v4)+len(v6) == 0 {
		return r, ErrNoObservers
	}
	var res4, res6 familyResult
	res4 = probeFamily(ctx, cfg, "udp4", v4, true)
	if len(v6) > 0 {
		res6 = probeFamily(ctx, cfg, "udp6", v6, false)
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	p := &r.Profile
	r.IPv4, r.IPv6 = res4.report, res6.report
	switch {
	case res4.answered:
		p.UDP, p.Mapping, p.Filtering, p.VaryingIP, p.Hairpin = UDPOK, res4.mapping, res4.filtering, res4.varyingIP, res4.hairpin
		p.Allocation, p.Delta, p.Jitter = res4.alloc, res4.delta, res4.jitter
		o := res4.primary
		r.Primary = &o
	case res4.tried:
		p.UDP = UDPBlocked
	}
	switch {
	case len(v6) == 0:
	case res6.answered:
		p.IPv6, p.Mapping6, p.Filtering6 = IPv6OK, res6.mapping, res6.filtering
	case res6.tried:
		p.IPv6 = IPv6Blocked
	default:
		p.IPv6 = IPv6Unavailable
	}
	p.Observers = uint8(min(255, res4.observers+res6.observers))
	if cfg.PortMap != nil {
		p.PortMap = cfg.PortMap(ctx) | PortMapChecked
	}
	r.Duration = time.Since(r.Started)
	return r, p.Validate()
}

type familyResult struct {
	report                     FamilyReport
	tried, answered, varyingIP bool
	mapping                    Mapping
	filtering                  Filtering
	alloc                      Allocation
	delta                      int16
	jitter                     uint16
	hairpin                    Hairpin
	observers                  int
	primary                    Observer
}

// orderObservers prefers observers with an alternate IP, then alternate port.
func orderObservers(obs []Observer, limit int) []Observer {
	obs = append([]Observer(nil), obs...)
	score := func(o Observer) int {
		s := 0
		if o.AltIP.IsValid() {
			s += 2
		}
		if o.AltPort.IsValid() {
			s++
		}
		return s
	}
	sort.SliceStable(obs, func(i, j int) bool { return score(obs[i]) > score(obs[j]) })
	if len(obs) > limit {
		obs = obs[:limit]
	}
	return obs
}

func probeFamily(ctx context.Context, cfg Config, network string, obs []Observer, v4 bool) (res familyResult) {
	if len(obs) == 0 {
		return res
	}
	obs = orderObservers(obs, cfg.MaxObservers)
	var socks []*sock
	defer func() {
		for _, s := range socks {
			s.close()
		}
	}()
	open := func() (*sock, error) {
		s, err := openSock(cfg, network)
		if err == nil {
			socks = append(socks, s)
		}
		return s, err
	}
	fail := func(err error) familyResult {
		res.report.Error = err.Error()
		return res
	}
	// Learn the routed source IP without sending anything.
	route, err := net.DialUDP(network, nil, net.UDPAddrFromAddrPort(obs[0].Primary))
	if err != nil {
		return fail(err)
	}
	localIP := route.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	route.Close()
	main, err := open()
	if err != nil {
		return fail(err)
	}
	local := netip.AddrPortFrom(localIP, main.local.Port())
	res.report.Local = local
	res.tried = true

	// Phase 1: the mapping socket asks every observer's primary endpoint.
	type answer struct {
		obs    Observer
		mapped netip.AddrPort
		cookie [16]byte
	}
	answers := make([]*answer, len(obs))
	var wg sync.WaitGroup
	for i, o := range obs {
		pr, err := start(main, main, o.Primary, o.Primary, packet{Type: typeRequest})
		if err != nil {
			res.report.Error = err.Error()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rep, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit); ok {
				answers[i] = &answer{o, rep.pkt.Observed, rep.pkt.Cookie}
			}
		}()
	}
	wg.Wait()
	var live []*answer
	for _, a := range answers {
		if a != nil {
			live = append(live, a)
		}
	}
	if len(live) == 0 || ctx.Err() != nil {
		return res
	}
	res.answered = true
	o1 := live[0]
	res.primary = o1.obs
	observed := map[netip.AddrPort]netip.AddrPort{}
	ips := map[netip.Addr]bool{}
	for _, a := range live {
		observed[a.obs.Primary] = a.mapped
		ips[a.obs.Primary.Addr()] = true
	}

	// Phase 2: fresh sockets map toward o1 back to back (allocation samples).
	// The first doubles as the filtering socket: it never contacts anything else.
	samples := make([]*sock, 0, cfg.AllocationSamples)
	n := cfg.AllocationSamples
	if !v4 {
		n = 1
	}
	for range n {
		s, err := open()
		if err != nil {
			return fail(err)
		}
		samples = append(samples, s)
	}
	probes := make([]*probe, len(samples))
	for i, s := range samples {
		if probes[i], err = start(s, s, o1.obs.Primary, o1.obs.Primary, packet{Type: typeRequest}); err != nil {
			res.report.Error = err.Error()
		}
	}
	mapped := make([]netip.AddrPort, len(samples))
	for i, pr := range probes {
		if pr == nil {
			continue
		}
		if rep, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit); ok {
			mapped[i] = rep.pkt.Observed
			res.report.Samples = append(res.report.Samples, Sample{netip.AddrPortFrom(localIP, samples[i].local.Port()), rep.pkt.Observed})
		}
	}
	filter := samples[0]
	filterMapped := mapped[0]

	// Phase 3, concurrently: remaining mapping observations, filtering, hairpin.
	type obsResult struct {
		ep, mapped netip.AddrPort
	}
	var mu sync.Mutex
	extra := []obsResult{}
	ev := Evidence{AltPort: "untested", AltIP: "untested", Hairpin: "untested"}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	for _, ep := range []netip.AddrPort{o1.obs.AltPort, o1.obs.AltIP} {
		if !ep.IsValid() {
			continue
		}
		pr, err := start(main, main, ep, ep, packet{Type: typeRequest})
		if err != nil {
			continue
		}
		run(func() {
			if rep, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit); ok {
				mu.Lock()
				extra = append(extra, obsResult{ep, rep.pkt.Observed})
				mu.Unlock()
			}
		})
	}
	outcome := func(ok bool) string {
		if ok {
			return "received"
		}
		return "missing"
	}
	if filterMapped.IsValid() {
		if o1.obs.AltPort.IsValid() {
			if pr, err := start(filter, filter, o1.obs.Primary, o1.obs.AltPort, packet{Type: typeRequest, Flags: FlagAltPort, Cookie: o1.cookie}); err == nil {
				run(func() {
					_, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit)
					mu.Lock()
					ev.AltPort = outcome(ok)
					mu.Unlock()
				})
			}
		}
		var ipProbe *probe
		if o1.obs.AltIP.IsValid() {
			ev.Via = "alt-ip"
			ipProbe, _ = start(filter, filter, o1.obs.Primary, o1.obs.AltIP, packet{Type: typeRequest, Flags: FlagAltIP, Cookie: o1.cookie})
		} else {
			// Cross-observer: another observer IP that the filter socket never
			// contacted sends to its mapping. The target must be on the
			// requester's own observed IP (the server enforces this).
			for _, a := range live[1:] {
				if a.obs.Primary.Addr() != o1.obs.Primary.Addr() && a.mapped.Addr() == filterMapped.Addr() {
					ev.Via = a.obs.Primary.String()
					ipProbe, _ = start(main, filter, a.obs.Primary, a.obs.Primary, packet{Type: typeRequest, TargetPort: filterMapped.Port(), Cookie: a.cookie})
					break
				}
			}
		}
		if ipProbe != nil {
			run(func() {
				_, ok := ipProbe.wait(ctx, cfg.Timeout, cfg.Retransmit)
				mu.Lock()
				ev.AltIP = outcome(ok)
				mu.Unlock()
			})
		}
	}
	if v4 && len(samples) >= 3 && mapped[1].IsValid() && mapped[2].IsValid() {
		// Hairpin: sample 1 sends to sample 2's external endpoint. Any source is
		// accepted (NATs differ in the hairpinned source); the txid is the proof.
		if pr, err := start(samples[1], samples[2], mapped[2], netip.AddrPort{}, packet{Type: typeHairpin}); err == nil {
			run(func() {
				_, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit)
				mu.Lock()
				ev.Hairpin = outcome(ok)
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return res
	}
	res.report.Filtering = ev

	// Classification.
	for _, e := range extra {
		observed[e.ep] = e.mapped
		ips[e.ep.Addr()] = true
	}
	res.observers = len(ips)
	var portDep, addrDep, haveSameIP, haveDiffIP bool
	eps := make([]netip.AddrPort, 0, len(observed))
	for ep := range observed {
		eps = append(eps, ep)
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].Compare(eps[j]) < 0 })
	first := observed[eps[0]]
	allLocal := true
	for i, a := range eps {
		m := observed[a]
		res.report.Observations = append(res.report.Observations, Observation{a, m})
		if m.Addr() != first.Addr() {
			res.varyingIP = true
		}
		if m != local {
			allLocal = false
		}
		for _, b := range eps[i+1:] {
			same := a.Addr() == b.Addr()
			eq := m == observed[b]
			haveSameIP = haveSameIP || same
			haveDiffIP = haveDiffIP || !same
			portDep = portDep || (same && !eq)
			addrDep = addrDep || (!same && !eq)
		}
	}
	for _, m := range mapped {
		if m.IsValid() && m.Addr() != first.Addr() {
			res.varyingIP = true
		}
	}
	switch {
	case portDep:
		res.mapping = MappingAddressPortDependent
	case addrDep && haveSameIP:
		res.mapping = MappingAddressDependent
	case addrDep:
		res.mapping = MappingEndpointDependent
	case allLocal:
		res.mapping = MappingNone
	case haveDiffIP:
		res.mapping = MappingEndpointIndependent
	}
	switch {
	case ev.AltPort == "received" && ev.AltIP == "received":
		res.filtering = FilterEndpointIndependent
	case ev.AltPort == "received" && ev.AltIP == "missing":
		res.filtering = FilterAddressDependent
	case ev.AltPort == "missing" && ev.AltIP != "received":
		res.filtering = FilterAddressPortDependent
	case ev.AltPort == "untested" && ev.AltIP == "received":
		res.filtering = FilterEndpointIndependent
	}
	if v4 {
		switch {
		case ev.Hairpin == "received":
			res.hairpin = HairpinYes
		case ev.Hairpin == "missing" && res.filtering == FilterEndpointIndependent:
			// Under address/port filtering a lost hairpin packet is ambiguous.
			res.hairpin = HairpinNo
		}
		if res.mapping != MappingNone {
			res.alloc, res.delta, res.jitter = classifyAllocation(res.report.Samples, cfg.MaxDelta, cfg.MaxJitter)
		}
	}
	return res
}

// classifyAllocation: preserving when most fresh sockets keep their local
// port; otherwise sequential when consecutive new mappings differ by a small
// stable delta (median), else random.
func classifyAllocation(samples []Sample, maxDelta, maxJitter int) (Allocation, int16, uint16) {
	if len(samples) < 3 {
		return AllocUnknown, 0, 0
	}
	kept := 0
	for _, s := range samples {
		if s.Local.Port() == s.Mapped.Port() {
			kept++
		}
	}
	if 2*kept > len(samples) {
		return AllocPreserving, 0, 0
	}
	deltas := make([]int, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		deltas = append(deltas, int(int16(samples[i].Mapped.Port()-samples[i-1].Mapped.Port())))
	}
	sorted := append([]int(nil), deltas...)
	sort.Ints(sorted)
	delta := sorted[(len(sorted)-1)/2]
	jitter := 0
	for _, d := range deltas {
		jitter = max(jitter, abs(d-delta))
	}
	if delta == 0 || abs(delta) > maxDelta || jitter > maxJitter {
		return AllocRandom, 0, 0
	}
	return AllocSequential, int16(delta), uint16(jitter)
}
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// MeasureLifetime runs the idle-then-probe ladder against the first answering
// IPv4 observer. Each rung is a fresh socket that maps once and then idles; at
// its rung time a separate control socket asks the observer to send from the
// endpoint the rung contacted to the rung's mapping (same tuple). The rung's
// own socket never sends again, so the probe cannot refresh its mapping.
func MeasureLifetime(ctx context.Context, cfg Config, observers []Observer) (Lifetime, error) {
	cfg = cfg.withDefaults()
	var v4 []Observer
	for _, o := range observers {
		if o.Validate() == nil && o.Primary.Addr().Is4() {
			v4 = append(v4, o)
		}
	}
	if len(v4) == 0 {
		return Lifetime{}, ErrNoObservers
	}
	var socks []*sock
	defer func() {
		for _, s := range socks {
			s.close()
		}
	}()
	open := func() (*sock, error) {
		s, err := openSock(cfg, "udp4")
		if err == nil {
			socks = append(socks, s)
		}
		return s, err
	}
	control, err := open()
	if err != nil {
		return Lifetime{}, err
	}
	var target Observer
	var cookie [16]byte
	plain := func(s *sock, o Observer) (reply, bool) {
		pr, err := start(s, s, o.Primary, o.Primary, packet{Type: typeRequest})
		if err != nil {
			return reply{}, false
		}
		return pr.wait(ctx, cfg.Timeout, cfg.Retransmit)
	}
	for _, o := range orderObservers(v4, cfg.MaxObservers) {
		if rep, ok := plain(control, o); ok {
			target, cookie = o, rep.pkt.Cookie
			break
		}
	}
	if !target.Primary.IsValid() {
		return Lifetime{}, fmt.Errorf("netcheck: no IPv4 observer answered")
	}
	type rung struct {
		after  time.Duration
		s      *sock
		mapped netip.AddrPort
		at     time.Time
	}
	// mapRungs opens one fresh socket per rung, maps them back to back, and
	// timestamps each mapping by its reply (the last packet through the NAT).
	mapRungs := func(after []time.Duration) ([]*rung, error) {
		rungs := make([]*rung, len(after))
		probes := make([]*probe, len(after))
		for i, d := range after {
			s, err := open()
			if err != nil {
				return nil, err
			}
			rungs[i] = &rung{after: d, s: s}
			if probes[i], err = start(s, s, target.Primary, target.Primary, packet{Type: typeRequest}); err != nil {
				return nil, err
			}
		}
		for i, pr := range probes {
			rep, ok := pr.wait(ctx, cfg.Timeout, cfg.Retransmit)
			if !ok {
				return nil, fmt.Errorf("netcheck: lifetime rung %d did not map", i)
			}
			rungs[i].mapped, rungs[i].at = rep.pkt.Observed, rep.at
		}
		return rungs, nil
	}
	probes := 0
	// alive waits until the rung is due, then asks the observer to send to it.
	alive := func(r *rung) (bool, error) {
		select {
		case <-time.After(time.Until(r.at.Add(r.after))):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if exp := time.Unix(int64(binary.BigEndian.Uint32(cookie[:])), 0); time.Until(exp) < 30*time.Second {
			if rep, ok := plain(control, target); ok {
				cookie = rep.pkt.Cookie
			}
		}
		probes++
		pr, err := start(control, r.s, target.Primary, target.Primary, packet{Type: typeRequest, TargetPort: r.mapped.Port(), Cookie: cookie})
		if err != nil {
			return false, err
		}
		_, ok := pr.wait(ctx, cfg.LifetimeTimeout, cfg.LifetimeTimeout/4)
		return ok, ctx.Err()
	}
	var coarse []time.Duration
	for d := cfg.LifetimeMin; ; d *= 2 {
		if d >= cfg.LifetimeMax {
			coarse = append(coarse, cfg.LifetimeMax)
			break
		}
		coarse = append(coarse, d)
	}
	rungs, err := mapRungs(coarse)
	if err != nil {
		return Lifetime{}, err
	}
	var low, high time.Duration
	for _, r := range rungs {
		ok, err := alive(r)
		if err != nil {
			return Lifetime{}, err
		}
		if !ok {
			high = r.after
			break
		}
		low = r.after
	}
	switch {
	case high == 0:
		return Lifetime{Low: low, Estimate: low, Bound: BoundAtLeast, Probes: probes}, nil
	case low == 0:
		return Lifetime{High: high, Estimate: high, Bound: BoundBelow, Probes: probes}, nil
	}
	ratio := float64(high) / float64(low)
	var fine []time.Duration
	for j := 1; j <= 3; j++ {
		fine = append(fine, time.Duration(float64(low)*math.Pow(ratio, float64(j)/4)))
	}
	if rungs, err = mapRungs(fine); err != nil {
		return Lifetime{}, err
	}
	for _, r := range rungs {
		ok, err := alive(r)
		if err != nil {
			return Lifetime{}, err
		}
		if !ok {
			high = r.after
			break
		}
		low = r.after
	}
	est := time.Duration(math.Sqrt(float64(low) * float64(high)))
	return Lifetime{Low: low, High: high, Estimate: est, Bound: BoundExact, Probes: probes}, nil
}

// sock is one probe socket with a single reader dispatching by transaction ID.
type sock struct {
	pc    net.PacketConn
	local netip.AddrPort
	mu    sync.Mutex
	waits map[[16]byte]chan reply
	done  chan struct{}
}
type reply struct {
	from netip.AddrPort
	pkt  packet
	at   time.Time
}

func openSock(cfg Config, network string) (*sock, error) {
	pc, err := cfg.ListenPacket(network)
	if err != nil {
		return nil, err
	}
	ua, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		pc.Close()
		return nil, errors.New("netcheck: probe socket is not UDP")
	}
	s := &sock{pc: pc, local: ua.AddrPort(), waits: make(map[[16]byte]chan reply), done: make(chan struct{})}
	go s.loop()
	return s, nil
}
func (s *sock) close() {
	s.pc.Close()
	<-s.done
}
func (s *sock) loop() {
	defer close(s.done)
	buf := make([]byte, PacketSize+1)
	for {
		n, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. ICMP-induced errors on some platforms
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		p, err := parsePacket(buf[:n])
		if err != nil || p.Type == typeRequest {
			continue
		}
		s.mu.Lock()
		ch := s.waits[p.TxID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- reply{netip.AddrPortFrom(ua.AddrPort().Addr().Unmap(), ua.AddrPort().Port()), p, time.Now()}:
			default:
			}
		}
	}
}

// probe is one request (retransmitted) whose reply is expected on recv from
// the exact endpoint expect (any source when expect is invalid: hairpin).
type probe struct {
	send, recv  *sock
	dst, expect netip.AddrPort
	wire        [PacketSize]byte
	tx          [16]byte
	ch          chan reply
}

func start(send, recv *sock, dst, expect netip.AddrPort, p packet) (*probe, error) {
	if _, err := rand.Read(p.TxID[:]); err != nil {
		return nil, err
	}
	pr := &probe{send: send, recv: recv, dst: dst, expect: expect, wire: p.marshal(), tx: p.TxID, ch: make(chan reply, 1)}
	recv.mu.Lock()
	recv.waits[p.TxID] = pr.ch
	recv.mu.Unlock()
	if err := pr.transmit(); err != nil {
		pr.release()
		return nil, err
	}
	return pr, nil
}
func (pr *probe) transmit() error {
	_, err := pr.send.pc.WriteTo(pr.wire[:], net.UDPAddrFromAddrPort(pr.dst))
	return err
}
func (pr *probe) release() {
	pr.recv.mu.Lock()
	delete(pr.recv.waits, pr.tx)
	pr.recv.mu.Unlock()
}
func (pr *probe) wait(ctx context.Context, timeout, retransmit time.Duration) (reply, bool) {
	defer pr.release()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(retransmit)
	defer tick.Stop()
	for {
		select {
		case r := <-pr.ch:
			if !pr.expect.IsValid() || r.from == netip.AddrPortFrom(pr.expect.Addr().Unmap(), pr.expect.Port()) {
				if pr.expect.IsValid() == (r.pkt.Type == typeResponse) {
					return r, true
				}
			}
		case <-tick.C:
			_ = pr.transmit()
		case <-deadline.C:
			return reply{}, false
		case <-ctx.Done():
			return reply{}, false
		}
	}
}
