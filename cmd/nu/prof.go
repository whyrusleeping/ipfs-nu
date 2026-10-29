//go:build nuprof

// Profiling for measurement builds only: `go build -tags nuprof ./cmd/nu`, then run with
// NU_CPUPROFILE=<file> and/or NU_MEMPROFILE=<file>. Profiles are written when the
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
	stopProfile = func() {
		if cpu != nil {
			pprof.StopCPUProfile()
			cpu.Close()
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
