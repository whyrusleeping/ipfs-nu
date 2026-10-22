package netcheck

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/netmon"
)

// MonitorConfig re-runs profiling on start, on network changes and periodically.
type MonitorConfig struct {
	Config
	// Observers returns the current observers (e.g. Fetch from connected relays).
	Observers func(context.Context) ([]Observer, error)
	// Interval between periodic quick probes (30 min); RetryInterval after a run
	// that had no observers or failed (1 min). The lifetime ladder runs on start,
	// after network changes and at most every LifetimeInterval (6 h).
	Interval, RetryInterval, LifetimeInterval time.Duration
	// OnChange, if set, is called (serially) after each published report.
	OnChange func(Report)
}

// Monitor owns the profiling schedule. Its zero report means "not measured yet".
type Monitor struct {
	cfg      MonitorConfig
	trigger  chan bool
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	current  Report
	have     bool
	lastErr  error
	lifetime *Lifetime
	measured time.Time
}

func StartMonitor(ctx context.Context, cfg MonitorConfig) (*Monitor, error) {
	if cfg.Observers == nil {
		return nil, errors.New("netcheck: monitor needs an observer source")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Minute
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = time.Minute
	}
	if cfg.LifetimeInterval <= 0 {
		cfg.LifetimeInterval = 6 * time.Hour
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Monitor{cfg: cfg, trigger: make(chan bool, 1), cancel: cancel, done: make(chan struct{})}
	go m.loop(ctx)
	return m, nil
}

// Trigger schedules a run. networkChanged also cancels an in-flight run and
// discards the previous lifetime (it belonged to the old network).
func (m *Monitor) Trigger(networkChanged bool) {
	for {
		select {
		case m.trigger <- networkChanged:
			return
		default:
		}
		select {
		case old := <-m.trigger:
			networkChanged = networkChanged || old
		default:
		}
	}
}

// Hook is a non-blocking netmon pipeline hook (register at ConfirmAddresses or
// RestartProbing); the run itself happens on the monitor goroutine.
func (m *Monitor) Hook() netmon.Hook {
	return func(context.Context, netmon.Event) error { m.Trigger(true); return nil }
}

// Current returns the latest report and the last run error, if any.
func (m *Monitor) Current() (Report, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current, m.have, m.lastErr
}

// Profile is suitable for ServeProfiles.
func (m *Monitor) Profile() (Profile, bool) {
	r, ok, _ := m.Current()
	return r.Profile, ok
}

func (m *Monitor) Close() {
	m.cancel()
	<-m.done
}

func (m *Monitor) loop(ctx context.Context) {
	defer close(m.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	changed := true
	pending := false
	var runCancel context.CancelFunc
	var runDone chan struct{}
	var runErr error
	for {
		select {
		case <-ctx.Done():
			if runCancel != nil {
				runCancel()
				<-runDone
			}
			return
		case c := <-m.trigger:
			changed = changed || c
			pending = true
			if c && runCancel != nil {
				runCancel()
			}
		case <-timer.C:
			pending = true
		case <-runDone:
			runCancel()
			runCancel, runDone = nil, nil
			if runErr != nil {
				timer.Reset(m.cfg.RetryInterval)
			} else {
				timer.Reset(m.cfg.Interval)
			}
		}
		if pending && runDone == nil {
			var runCtx context.Context
			runCtx, runCancel = context.WithCancel(ctx)
			runDone = make(chan struct{})
			go func(c bool, done chan struct{}) {
				defer close(done)
				runErr = m.run(runCtx, c)
			}(changed, runDone)
			pending, changed = false, false
		}
	}
}

// run returns an error when the quick profile could not be produced.
func (m *Monitor) run(ctx context.Context, changed bool) error {
	setErr := func(err error) error {
		if ctx.Err() == nil {
			m.mu.Lock()
			m.lastErr = err
			m.mu.Unlock()
		}
		return err
	}
	obs, err := m.cfg.Observers(ctx)
	if err == nil && len(obs) == 0 {
		err = ErrNoObservers
	}
	if err != nil {
		return setErr(err)
	}
	r, err := Probe(ctx, m.cfg.Config, obs)
	if err != nil {
		return setErr(err)
	}
	m.mu.Lock()
	if changed {
		m.lifetime = nil
	}
	carry := m.lifetime
	due := carry == nil || time.Since(m.measured) >= m.cfg.LifetimeInterval
	m.mu.Unlock()
	if carry != nil {
		r.SetLifetime(*carry)
	}
	m.publish(r, nil)
	if r.Profile.UDP != UDPOK || !due {
		return nil
	}
	l, err := MeasureLifetime(ctx, m.cfg.Config, obs)
	if err != nil {
		setErr(err) // the quick profile stands; retry the ladder next run
		return nil
	}
	r.SetLifetime(l)
	m.mu.Lock()
	m.lifetime, m.measured = &l, time.Now()
	m.mu.Unlock()
	m.publish(r, nil)
	return nil
}

func (m *Monitor) publish(r Report, err error) {
	m.mu.Lock()
	m.current, m.have, m.lastErr = r, true, err
	m.mu.Unlock()
	if m.cfg.OnChange != nil {
		m.cfg.OnChange(r)
	}
}
