// Package timeouts is the one knob for wall-clock ceilings in tests. A ceiling only
// catches hangs: a correct run never approaches it, so it is generous rather than
// tight, and NU_TEST_TIMEOUT_SCALE (a float, default 1) stretches every ceiling on a
// slow or loaded machine. It is a leaf package so transport-level tests can use it.
package timeouts

import (
	"os"
	"strconv"
	"time"
)

// Scale multiplies d by NU_TEST_TIMEOUT_SCALE (default 1).
func Scale(d time.Duration) time.Duration {
	if f, err := strconv.ParseFloat(os.Getenv("NU_TEST_TIMEOUT_SCALE"), 64); err == nil && f > 0 {
		return time.Duration(float64(d) * f)
	}
	return d
}

// Ceiling is the default hang ceiling for a wait or a test-scoped context (30 s scaled).
func Ceiling() time.Duration { return Scale(30 * time.Second) }
