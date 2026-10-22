package extract

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

func file(p, data string, mode uint32) Entry {
	return Entry{Path: p, Kind: KindFile, Mode: mode, Size: int64(len(data)),
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil }}
}

func walkOf(entries ...Entry) Walk {
	return func(yield func(Entry, error) bool) {
		for _, e := range entries {
			if !yield(e, nil) {
				return
			}
		}
	}
}

// A good tree extracts with bounded modes and skipped symlinks; hostile paths and
// symlinks in the destination are refused before anything escapes.
func TestExtractSafety(t *testing.T) {
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out")
	rep, err := Extract(ctx, dest, walkOf(
		Entry{Path: "", Kind: KindDir},
		file("a.txt", "hello", 0o644),
		Entry{Path: "bin", Kind: KindDir},
		file("bin/run", "#!/bin/sh", 0o777),
		Entry{Path: "ln", Kind: KindSymlink, Target: "/etc/passwd"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 2 || rep.Dirs != 2 || rep.Bytes != 14 || len(rep.Skipped) != 1 || rep.Skipped[0].Path != "ln" {
		t.Fatalf("report: %+v", rep)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "a.txt")); fi.Mode().Perm() != FileMode {
		t.Fatalf("a.txt mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dest, "bin/run")); fi.Mode().Perm() != ExecMode {
		t.Fatalf("bin/run mode %v", fi.Mode())
	}
	if _, err := os.Lstat(filepath.Join(dest, "ln")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink was created: %v", err)
	}
	if names, _ := filepath.Glob(filepath.Join(dest, ".nu-extract-*")); len(names) != 0 {
		t.Fatalf("temp files left: %v", names)
	}
	// Re-extracting replaces files in place; names that differ only by case are distinct
	// entries here (a case-insensitive or normalizing volume reports them as skipped with
	// the entry they map onto, never merging silently — checked by hand on macOS).
	rep, err = Extract(ctx, dest, walkOf(file("a.txt", "again", 0), file("A.txt", "upper", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(b) != "again" {
		t.Fatalf("a.txt not replaced: %q", b)
	}
	if rep.Files+len(rep.Skipped) != 2 {
		t.Fatalf("case-variant names: %+v", rep)
	}
	for _, sk := range rep.Skipped {
		if !strings.Contains(sk.Reason, "a.txt") {
			t.Fatalf("skip reason must name the colliding entry: %+v", sk)
		}
	}
	// NFC and NFD spellings are distinct tree entries: both written where the filesystem
	// keeps them apart (Linux), the second reported naming the first where it treats them
	// as one name (APFS; this runs in the macOS CI job). The first keeps its own bytes.
	nfc, nfd := "caf\u00e9", "cafe\u0301"
	rep, err = Extract(ctx, dest, walkOf(file(nfc, "nfc", 0), file(nfd, "nfd", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, nfc)); string(b) != "nfc" {
		t.Fatalf("%q holds %q", nfc, b)
	}
	switch len(rep.Skipped) {
	case 0:
		if b, _ := os.ReadFile(filepath.Join(dest, nfd)); string(b) != "nfd" || rep.Files != 2 {
			t.Fatalf("%q holds %q (%+v)", nfd, b, rep)
		}
	case 1:
		if sk := rep.Skipped[0]; sk.Path != nfd || !strings.Contains(sk.Reason, nfc) {
			t.Fatalf("normalization collision must name %q: %+v", nfc, sk)
		}
	default:
		t.Fatalf("report: %+v", rep)
	}

	// Hostile paths never produce a write.
	outside := filepath.Join(t.TempDir(), "victim")
	bad := []Entry{
		file("../victim", "x", 0),
		file("/victim", "x", 0),
		file("a/../../victim", "x", 0),
		file("a\\..\\victim", "x", 0),
		file("a/\x00/victim", "x", 0),
		file("./victim", "x", 0),
	}
	for _, e := range bad {
		_, err := Extract(ctx, dest, walkOf(e))
		if !isUnsafe(err) {
			t.Errorf("%q: %v", e.Path, err)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped: %v", err)
	}

	// A symlink already in the destination is not followed, whether it names the
	// directory being written into or the file being replaced.
	victimDir := t.TempDir()
	if err := os.Symlink(victimDir, filepath.Join(dest, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victimDir, "f"), filepath.Join(dest, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{file("escape/pwned", "x", 0), file("escape.txt", "x", 0)} {
		if _, err := Extract(ctx, dest, walkOf(e)); !isUnsafe(err) || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%q: %v", e.Path, err)
		}
	}
	if entries, _ := os.ReadDir(victimDir); len(entries) != 0 {
		t.Fatalf("wrote through a symlink: %v", entries)
	}

	// X1 is all-or-nothing (0077): an unsafe path late in the walk means nothing at all
	// is written, not the entries before it.
	fresh := filepath.Join(t.TempDir(), "fresh")
	if _, err := Extract(ctx, fresh, walkOf(Entry{Path: "", Kind: KindDir}, file("good.txt", "x", 0), file("z\\..\\evil", "x", 0))); !isUnsafe(err) {
		t.Fatalf("late unsafe path: %v", err)
	}
	if _, err := os.Lstat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something was written before the unsafe path was refused: %v", err)
	}

	// TOCTOU: a directory swapped for a symlink after the checks (here, while the file's
	// bytes are opened) still cannot redirect the write: it goes through an os.Root.
	race := filepath.Join(t.TempDir(), "race")
	victim2 := t.TempDir()
	swap := file("d/x", "x", 0)
	open := swap.Open
	swap.Open = func(ctx context.Context) (io.ReadCloser, error) {
		os.Remove(filepath.Join(race, "d"))
		if err := os.Symlink(victim2, filepath.Join(race, "d")); err != nil {
			t.Fatal(err)
		}
		return open(ctx)
	}
	if _, err := Extract(ctx, race, walkOf(Entry{Path: "", Kind: KindDir}, Entry{Path: "d", Kind: KindDir}, swap)); err == nil {
		t.Fatal("write through a swapped-in symlink succeeded")
	}
	if entries, _ := os.ReadDir(victim2); len(entries) != 0 {
		t.Fatalf("wrote through a swapped-in symlink: %v", entries)
	}
}

func isUnsafe(err error) bool {
	var ae *apiv0.Error
	return errors.As(err, &ae) && ae.Code == apiv0.CodeInvalidArgument && strings.Contains(ae.Message, "unsafe path") && ae.Action != ""
}
