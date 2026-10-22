//go:build !linux

package host

// uplinkOffload is unknown here; quic-go uses GSO only on Linux.
func uplinkOffload() (string, bool, error) { return "", false, nil }
