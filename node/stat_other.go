//go:build !unix

package node

import "io/fs"

// inode is unavailable here; the import cache keys on path, size and mtime alone.
func inode(fi fs.FileInfo) uint64 { return 0 }
