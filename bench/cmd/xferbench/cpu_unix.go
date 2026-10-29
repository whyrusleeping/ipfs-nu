//go:build unix

package main

import (
	"syscall"
	"time"
)

// cpuTime is the process's user plus system CPU time.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
