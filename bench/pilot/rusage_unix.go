//go:build unix

package pilot

import (
	"runtime"
	"syscall"
	"time"
)

// CPU is the process's user+system CPU time so far.
func CPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// MaxRSS is the process's peak resident set in bytes.
func MaxRSS() int64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss) // bytes on macOS
	}
	return int64(ru.Maxrss) << 10 // KiB on Linux
}
