//go:build !unix

package store

// freeSpace is unknown on this platform.
func freeSpace(string) int64 { return -1 }
