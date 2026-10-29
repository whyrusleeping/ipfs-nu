//go:build nuprof

// Profiling for measurement builds only: `go build -tags nuprof ./cmd/nu`, then run with
// NU_CPUPROFILE=<file>, NU_MEMPROFILE=<file> and/or NU_BLOCKPROFILE=<file> (waits of
// 100 µs or more: locks, channels, the database connection). Profiles are written when the
// command returns (a daemon on Ctrl-C).
package main

import (
	"os"
	"runtime"
	"runtime/pprof"
)

func init() {
	var cpu *os.File
	if p := os.Getenv("NU_CPUPROFILE"); p != "" {
		f, err := os.Create(p)
		if err == nil && pprof.StartCPUProfile(f) == nil {
			cpu = f
		}
	}
	if os.Getenv("NU_BLOCKPROFILE") != "" {
		runtime.SetBlockProfileRate(100_000)
	}
	stopProfile = func() {
		if cpu != nil {
			pprof.StopCPUProfile()
			cpu.Close()
		}
		if p := os.Getenv("NU_BLOCKPROFILE"); p != "" {
			if f, err := os.Create(p); err == nil {
				_ = pprof.Lookup("block").WriteTo(f, 0)
				f.Close()
			}
		}
		if p := os.Getenv("NU_MEMPROFILE"); p != "" {
			if f, err := os.Create(p); err == nil {
				runtime.GC()
				_ = pprof.Lookup("allocs").WriteTo(f, 0)
				f.Close()
			}
		}
	}
}
