//go:build !unix

package pilot

import "time"

// CPU is unavailable on this platform.
func CPU() time.Duration { return 0 }

// MaxRSS is unavailable on this platform.
func MaxRSS() int64 { return 0 }
