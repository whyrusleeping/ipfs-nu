// Package slow gates opt-in slow tests (multi-GB files, long property runs, soak) behind
// NU_SLOW=1, per CONTRIBUTING.md.
package slow

import (
	"os"
	"testing"
)

// Enabled reports whether slow tests were requested.
func Enabled() bool { return os.Getenv("NU_SLOW") == "1" }

// Skip skips t unless NU_SLOW=1.
func Skip(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skip("slow test; set NU_SLOW=1 to run")
	}
}
