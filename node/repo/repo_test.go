package repo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A repo whose layout version is newer than this build is refused untouched (U4).
func TestNewerLayoutRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nu")
	if _, _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte("99\n"), FileMode); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(dir); !errors.Is(err, ErrNewerLayout) {
		t.Fatalf("newer layout: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, VersionFile)); string(b) != "99\n" {
		t.Fatalf("refused open rewrote the version file: %q", b)
	}
}
