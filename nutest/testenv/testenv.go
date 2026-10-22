// Package testenv shapes the test process for a shared machine (0074). Importing it
// (blank) from the test files of a package:
//
//		import _ "github.com/whyrusleeping/nu/nutest/testenv"
//
//	  - points the temp directory (os.TempDir, hence testing.T.TempDir) at tmpfs when
//	    /dev/shm is writable, so stores created by tests do not fsync against a disk
//	    shared with other work: on the dev box a loaded disk turned 3 s packages into
//	    70 s ones and timed out tests whose only I/O was a few hundred KiB. Skipped when
//	    TMPDIR is set (an explicit choice wins) or under NU_SLOW=1 (multi-GB opt-in tests
//	    must not fill RAM);
//	  - caps GOMAXPROCS at 16 unless the GOMAXPROCS variable is set: on a 192-core host
//	    every test binary otherwise runs 192 Ps whose idle scheduling burned a third of
//	    the suite's CPU for no wall-clock gain.
//
// nutest imports it, so every nutest-based package gets both.
package testenv

import (
	"os"
	"runtime"
)

// TempDir is the directory tests were pointed at, or "" when the default was kept.
var TempDir string

// MaxProcs is the GOMAXPROCS cap applied to test binaries.
const MaxProcs = 16

func init() {
	if os.Getenv("GOMAXPROCS") == "" && runtime.NumCPU() > MaxProcs {
		runtime.GOMAXPROCS(MaxProcs)
	}
	if os.Getenv("TMPDIR") != "" || os.Getenv("NU_SLOW") == "1" {
		return
	}
	const shm = "/dev/shm"
	if st, err := os.Stat(shm); err != nil || !st.IsDir() {
		return
	}
	probe, err := os.MkdirTemp(shm, "nu-test-probe-*")
	if err != nil {
		return
	}
	_ = os.Remove(probe)
	if err := os.Setenv("TMPDIR", shm); err == nil {
		TempDir = shm
	}
}
