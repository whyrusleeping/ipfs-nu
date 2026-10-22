package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "nu.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func userVersion(t *testing.T, dir string) int {
	t.Helper()
	db := rawDB(t, dir)
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Upgrades and compatibility (operations.md U2–U4): a store at an older schema whose
// upgrade process is killed inside the last migration stays at the previous version and
// the next Open resumes and completes it; a store from a newer build is refused with
// ErrNewerSchema and left untouched.
func TestInterruptedUpgrade(t *testing.T) {
	if dir := os.Getenv("NU_MIGRATE_CRASH_DIR"); dir != "" {
		// Child: die (SIGKILL on Unix) inside the final migration's transaction.
		migrateHook = func(v int) {
			if v == len(migrations) {
				p, _ := os.FindProcess(os.Getpid())
				_ = p.Kill()
				select {}
			}
		}
		_, err := Open(dir, Options{})
		t.Fatalf("child survived the migration: %v", err)
	}
	dir := t.TempDir()
	db := rawDB(t, dir)
	// Start before migration 3 (partials.held) so the resumed upgrade has data to fill;
	// the child completes 3 and dies inside the last one.
	prev := len(migrations) - 1
	if err := migrateTo(db, 2); err != nil {
		t.Fatal(err)
	}
	groups := []byte{0b101}
	const size = 40000
	if _, err := db.Exec(`INSERT INTO partials (mh, size, groups, updated) VALUES (?, ?, ?, 0)`, []byte{1, 2, 3}, size, groups); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptedUpgrade$")
	cmd.Env = append(os.Environ(), "NU_MIGRATE_CRASH_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("child exited cleanly:\n%s", out)
	}
	if v := userVersion(t, dir); v != prev {
		t.Fatalf("after the killed upgrade the schema is %d, want %d (the last completed migration)", v, prev)
	}

	s := open(t, dir)
	if v := userVersion(t, dir); v != SchemaVersion {
		t.Fatalf("resumed upgrade reached %d, want %d", v, SchemaVersion)
	}
	var held int64
	if err := s.db.QueryRow(`SELECT held FROM partials`).Scan(&held); err != nil || held != heldBytes(groups, size) || held == 0 {
		t.Fatalf("migrated partial held = %d (%v), want %d", held, err, heldBytes(groups, size))
	}
	st, err := s.Stats()
	if err != nil || st.Partials != held {
		t.Fatalf("usage counters after resume: %+v %v", st, err)
	}
	s.Close()

	// A newer schema is refused without modification.
	db = rawDB(t, dir)
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+5)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema: %v", err)
	}
	if v := userVersion(t, dir); v != SchemaVersion+5 {
		t.Fatalf("refused open changed the schema version to %d", v)
	}
}
