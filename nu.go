// Package nu holds the product name in one place. The name is provisional; keep every
// user-facing use of it derived from these constants so a rename stays mechanical.
package nu

const (
	// Name is the binary and user-facing product name.
	Name = "nu"
	// EnvPrefix prefixes every environment variable the product reads.
	EnvPrefix = "NU_"
)
