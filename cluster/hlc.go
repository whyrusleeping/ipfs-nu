package cluster

import (
	"fmt"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// HLC is a hybrid logical clock value: microseconds since the Unix epoch, with the
// logical counter folded into the sub-millisecond digits. It is written as a TID string
// with clock ID 0 (docs/cluster.md "State encoding and evaluation").
type HLC uint64

// String returns the TID form.
func (h HLC) String() string { return syntax.NewTIDFromInteger(uint64(h) << 10).String() }

// Time is the physical part (ordering metadata, not a trusted timestamp).
func (h HLC) Time() time.Time { return time.UnixMicro(int64(h)) }

// ParseHLC parses a TID-form HLC; the clock-ID bits must be zero.
func ParseHLC(s string) (HLC, error) {
	t, err := syntax.ParseTID(s)
	if err != nil {
		return 0, fmt.Errorf("cluster: hlc: %w", err)
	}
	if t.ClockID() != 0 {
		return 0, fmt.Errorf("cluster: hlc %s has a clock ID", s)
	}
	return HLC(t.Integer() >> 10), nil
}

// Clock issues HLC values: max(now, last+1, seen+1).
type Clock struct {
	mu   sync.Mutex
	last HLC
	Now  func() time.Time // nil means time.Now
}

// Observe raises the clock to at least h (a value seen in merged state).
func (c *Clock) Observe(h HLC) {
	c.mu.Lock()
	if h > c.last {
		c.last = h
	}
	c.mu.Unlock()
}

// Next returns a value greater than every value issued or observed.
func (c *Clock) Next() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	h := HLC(now().UnixMicro())
	if h <= c.last {
		h = c.last + 1
	}
	c.last = h
	return h
}

func parseTID(s string) (uint64, error) {
	t, err := syntax.ParseTID(s)
	if err != nil {
		return 0, err
	}
	return t.Integer(), nil
}
