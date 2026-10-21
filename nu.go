// Package nu holds the product name in one place. The name is provisional; keep every
// user-facing use of it derived from these constants so a rename stays mechanical.
package nu

const (
	// Name is the binary and user-facing product name.
	Name = "nu"
	// EnvPrefix prefixes every environment variable the product reads.
	EnvPrefix = "NU_"
	// DefaultDir is the default `$NU_PATH` directory name under the user's home directory.
	DefaultDir = ".nu"
)

// Version is the build version reported by `nu version`, `nu status` and the API. Release
// builds set it with `-ldflags "-X github.com/whyrusleeping/nu.Version=v0.1.0"`.
var Version = "dev"
