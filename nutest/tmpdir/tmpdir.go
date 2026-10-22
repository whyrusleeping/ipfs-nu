// Package tmpdir points the test temp directory (os.TempDir, hence testing.T.TempDir)
// at tmpfs when one is available, so stores created by tests do not fsync against a disk
// shared with other work. On the shared dev machine a loaded disk turned 3 s packages
// into 70 s ones and timed out tests whose only I/O was a few hundred KiB (0074).
//
// It is a no-op when TMPDIR is already set (an explicit choice wins), when NU_SLOW=1
// (multi-GB opt-in tests must not fill RAM) or when /dev/shm is not a writable
// directory. Import it (blank) from the test files of packages that create stores:
//
//	import _ "github.com/whyrusleeping/nu/nutest/tmpdir"
package tmpdir

import (
	"os"
	"path/filepath"
)

// Dir is the directory tests were pointed at, or "" when the default was kept.
var Dir string

func init() {
	if os.Getenv("TMPDIR") != "" || os.Getenv("NU_SLOW") == "1" {
		return
	}
	const shm = "/dev/shm"
	st, err := os.Stat(shm)
	if err != nil || !st.IsDir() {
		return
	}
	probe, err := os.MkdirTemp(shm, "nu-test-probe-*")
	if err != nil {
		return
	}
	_ = os.Remove(probe)
	if err := os.Setenv("TMPDIR", filepath.Clean(shm)); err == nil {
		Dir = shm
	}
}
