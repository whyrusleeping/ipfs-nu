//go:build unix

package node

import (
	"io/fs"
	"syscall"
)

// inode returns the file's inode number, part of the import-cache key.
func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
