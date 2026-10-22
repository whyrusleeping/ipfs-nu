//go:build unix

package node

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// fsTraits probes how the filesystem under dir treats names: whether it folds case and
// whether it treats NFC and NFD spellings as one name (APFS on macOS does both).
func fsTraits(t *testing.T, dir string) (foldsCase, foldsUnicode bool) {
	t.Helper()
	p := t.TempDir()
	if dir != "" {
		var err error
		if p, err = os.MkdirTemp(dir, "probe"); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(p)
	}
	os.WriteFile(filepath.Join(p, "probe"), nil, 0o644)
	_, err := os.Lstat(filepath.Join(p, "PROBE"))
	foldsCase = err == nil
	os.WriteFile(filepath.Join(p, "\u00e9"), nil, 0o644)
	_, err = os.Lstat(filepath.Join(p, "e\u0301"))
	foldsUnicode = err == nil
	return
}

// Filename, Unicode, case, symlink and mode handling (0044; data.md "Entries",
// operations.md X1–X6) through add and get -o on the machine's own filesystem. Names are
// kept byte-exact (NFC and NFD spellings are different entries; nothing is normalized),
// case variants are distinct entries, a symlink is recorded on add and reported (not
// created) on extract, a FIFO is skipped and reported on add, only the executable bit
// survives, and a non-UTF-8 name fails the add naming the file. Where the destination
// filesystem folds case or Unicode (macOS APFS), the second spelling is reported as
// skipped naming the entry it maps onto and the first keeps its own bytes: never a silent
// merge. On Linux (ext4/tmpfs) every file round-trips byte-exact.
func TestFilenameRoundTrip(t *testing.T) {
	src := t.TempDir()
	nfc, nfd := "caf\u00e9.txt", "cafe\u0301.txt"
	long := strings.Repeat("\u00e9", 127) // 254 bytes
	files := map[string]string{
		nfc:                  "nfc",
		nfd:                  "nfd",
		"Readme.md":          "mixed",
		"README.md":          "upper",
		"with space & 🙂.txt": "emoji",
		"sub/" + long:        "long",
		"run.sh":             "#!/bin/sh\n",
		"private.bin":        "secret",
	}
	foldsCase, foldsUnicode := fsTraits(t, src)
	for name, body := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if _, err := os.Lstat(p); err == nil {
			// The source filesystem itself merged this spelling; it cannot hold both.
			delete(files, name)
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(filepath.Join(src, "run.sh"), 0o750)
	os.Chmod(filepath.Join(src, "private.bin"), 0o600)
	if err := os.Symlink(nfc, filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	n := newLocal(t)
	res, err := add(t, n, apiv0.AddRequest{Path: src, Recursive: true, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.HasSuffix(res.Skipped[0], "pipe") {
		t.Fatalf("add must report the FIFO and nothing else: %v", res.Skipped)
	}

	out := filepath.Join(t.TempDir(), "out")
	var gr apiv0.GetResult
	if err := runOp(t, n, func() (*apiv0.Operation, error) {
		return n.Get(t.Context(), apiv0.GetRequest{Target: res.CID, Output: out})
	}, &gr); err != nil {
		t.Fatal(err)
	}
	skipped := strings.Join(gr.Skipped, "\n")
	if !strings.Contains(skipped, "link") || !strings.Contains(skipped, "symlink") || !strings.Contains(skipped, nfc) {
		t.Fatalf("symlink must be reported with its target: %v", gr.Skipped)
	}
	if _, err := os.Lstat(filepath.Join(out, "link")); !os.IsNotExist(err) {
		t.Fatal("a symlink was created")
	}
	outCase, outUnicode := fsTraits(t, filepath.Dir(out))
	for name, body := range files {
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name)))
		var reported bool
		for _, s := range gr.Skipped {
			if strings.HasPrefix(s, name) && strings.Contains(s, "maps onto existing entry") {
				reported = true
			}
		}
		switch {
		case reported && (outCase || outUnicode):
			// Skipped by name mapping: the entry it collided with keeps its own bytes,
			// checked in its own iteration.
		case err != nil || string(b) != body:
			t.Errorf("%q: %q, %v (reported: %v)", name, b, err, gr.Skipped)
		}
	}
	if !outCase && !outUnicode && len(gr.Skipped) != 1 {
		t.Fatalf("on a case- and normalization-sensitive filesystem only the symlink is skipped: %v", gr.Skipped)
	}
	if (outCase && !foldsCase) || (outUnicode && !foldsUnicode) {
		t.Log("destination folds names differently from the source filesystem")
	}
	for name, want := range map[string]os.FileMode{"run.sh": 0o755, "private.bin": 0o644} {
		if fi, err := os.Stat(filepath.Join(out, name)); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", name, fi.Mode().Perm(), want)
		}
	}

	// A name that is not UTF-8 is refused by add, naming the file (Linux filesystems
	// accept such names; APFS refuses to create them, so there is nothing to test).
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "latin1-\xe9.txt"), []byte("x"), 0o644); err == nil {
		if _, err := add(t, n, apiv0.AddRequest{Path: bad, Recursive: true}); err == nil || !strings.Contains(err.Error(), "rename or exclude") {
			t.Fatalf("non-UTF-8 name: %v", err)
		}
	}
}
