//go:build !unix

package main

import "time"

// cpuTime is not measured on this platform.
func cpuTime() time.Duration { return 0 }
