//go:build !unix

package repo

import "os"

// Non-unix platforms are not supported targets; the lock degrades to a PID file.
func flock(*os.File) error   { return nil }
func funlock(*os.File) error { return nil }
