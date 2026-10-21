//go:build unix

package store

import "syscall"

// freeSpace returns the bytes available to this user on the filesystem holding dir, or
// -1 when unknown.
func freeSpace(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert // field widths differ per OS
}
