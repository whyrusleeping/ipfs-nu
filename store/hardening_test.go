package store

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/nutest/slow"
)

// Real process death, not injected return errors. The seed chooses delays from the
// child's operation-start handshake; filesystem/SQLite cut points are nondeterministic.
func TestHardeningCrashCampaign(t *testing.T) {
	slow.Skip(t)
	if dir := os.Getenv("NU_CRASH_CAMPAIGN_DIR"); dir != "" {
		crashCampaignChild(t, dir, os.Getenv("NU_CRASH_CAMPAIGN_PHASE"))
		return
	}
	rng := rand.New(rand.NewPCG(68, 1))
	for _, phase := range []string{"add", "pin", "gc", "switch"} {
		for round := range 12 {
			t.Run(fmt.Sprintf("%s/%02d", phase, round), func(t *testing.T) {
				dir := t.TempDir()
				s := open(t, dir)
				for i := range 32 {
					c, err := s.AddBlob(pat(int64(i), 128<<10), 128<<10)
					if err != nil {
						t.Fatal(err)
					}
					if i < 2 {
						if err := s.Pin(Pin{CID: c, Mode: Direct, Source: SourceExplicit, Name: fmt.Sprint(i)}); err != nil {
							t.Fatal(err)
						}
					}
				}
				s.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHardeningCrashCampaign$")
				cmd.Env = append(os.Environ(), "NU_CRASH_CAMPAIGN_DIR="+dir, "NU_CRASH_CAMPAIGN_PHASE="+phase)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cmd.Process.Kill() }()
				scanner := bufio.NewScanner(out)
				ready := false
				for scanner.Scan() {
					if scanner.Text() == "ready" {
						ready = true
						break
					}
				}
				if !ready {
					_ = cmd.Wait()
					t.Fatalf("child never ready: %s", stderr.String())
				}
				delay := time.Duration(rng.IntN(20000)) * time.Microsecond
				timer := time.NewTimer(delay)
				<-timer.C
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != -1 {
					t.Fatalf("child exited instead of being killed: %v; %s", err, stderr.String())
				}
				s = open(t, dir)
				checkCrashStore(t, s)
				if _, err := s.GC(context.Background(), GCOptions{}); err != nil {
					t.Fatal(err)
				}
				checkCrashStore(t, s)
				if n, err := s.sweepOrphans(time.Now().UnixNano()); err != nil || n != 0 {
					t.Fatalf("GC left %d orphans: %v", n, err)
				}
				t.Logf("seed=68/1 phase=%s kill_after=%s", phase, delay)
			})
		}
	}
}

func crashCampaignChild(t *testing.T, dir, phase string) {
	s := open(t, dir)
	pins, err := s.Pins()
	if err != nil || len(pins) != 2 {
		t.Fatal("initial pins", err)
	}
	fmt.Println("ready")
	for i := 0; ; i++ {
		var err error
		switch phase {
		case "add":
			_, err = s.AddBlob(pat(int64(i+100), 16<<20), 16<<20)
		case "pin":
			p := pins[i%2]
			p.Name = "churn"
			if err = s.Pin(p); err == nil {
				_, err = s.Unpin(p)
			}
		case "gc":
			_, err = s.GC(context.Background(), GCOptions{})
		case "switch":
			// Safe caller-side pin-new before unpin-old; not an atomic named-head API.
			next, old := pins[i%2], pins[(i+1)%2]
			next.Name = "switch"
			old.Name = "switch"
			if err = s.Pin(next); err == nil {
				_, err = s.Unpin(old)
			}
		default:
			t.Fatal("bad phase")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func checkCrashStore(t *testing.T, s *Store) {
	t.Helper()
	var integrity string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %s: %v", integrity, err)
	}
	pins, err := s.Pins()
	if err != nil || len(pins) < 2 {
		t.Fatal("durable pins lost", err)
	}
	for _, p := range pins {
		w, err := s.Walk(context.Background(), p, nil)
		if err != nil || !w.Complete() {
			t.Fatalf("dangling pin: %+v %v", p, err)
		}
	}
	rows, err := s.db.Query("SELECT mh FROM blobs")
	if err != nil {
		t.Fatal(err)
	}
	var hashes []cid.Multihash
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		mh, err := cid.DecodeMultihash(b)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, mh)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, mh := range hashes {
		b, err := s.OpenBlob(mh)
		if err != nil {
			t.Fatal(err)
		}
		got, size, err := blob.Sum(b.Reader())
		if err != nil || got != mh {
			b.Close()
			t.Fatalf("partial or corrupt blob %s: %v", cid.New(cid.Raw, mh), err)
		}
		var sl bytes.Buffer
		r := blob.Range{Start: 0, End: blob.Groups(size)}
		if err := b.ExtractSlice(&sl, r); err != nil {
			t.Fatal(err)
		}
		b.Close()
		if err := blob.DecodeSlice(&sl, mh, size, r, blob.Sink{}); err != nil {
			t.Fatal("corrupt outboard", err)
		}
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if rc := recount(t, s); !sameClasses(st, rc) {
		t.Fatalf("accounting %+v recount %+v", st, rc)
	}
	if st.Objects != int64(len(hashes)) {
		t.Fatalf("objects %d rows %d", st.Objects, len(hashes))
	}
	if ents, err := os.ReadDir(filepath.Join(s.dir, "staging")); err != nil || len(ents) != 0 {
		t.Fatal("abandoned staging", err)
	}
	if st.Staging != 0 || st.Partials != 0 {
		t.Fatal("partial accounting", st)
	}
}
