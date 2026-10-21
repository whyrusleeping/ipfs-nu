//go:build !linux

package magicsock

// quic-go does not enable UDP GSO here. Physical-address writes still retain OOB.
func innerOOB(_ []byte) ([]byte, int, error) { return nil, 0, nil }
