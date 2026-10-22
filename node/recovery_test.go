package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/repo"
)

// Clean-machine restore (0044): an archive dataset with two versions and a blob above
// the CAR block limit is exported as a content bundle and its authority backed up; the
// repo is wiped; restoring the authority verifies the signed cluster state under the same
// identity, importing the bundle verifies every object and restores the pins, and both
// versions read back byte-exact. A truncated bundle pins nothing; a restore never
// replaces another node key.
func TestCleanMachineRestore(t *testing.T) {
	ctx := context.Background()
	n := newLocal(t)
	src := t.TempDir()
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "photos", Archive: true, SourcePath: src}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, src, map[string]string{"a.txt": "version one"})
	v1 := importDir(t, n, "photos", src).Version
	big := bytes.Repeat([]byte("0123456789abcdef"), (9<<20)/16) // 9 MiB: stored beside the CAR
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, src, map[string]string{"a.txt": "version two"})
	v2 := importDir(t, n, "photos", src).Version
	peer := n.PeerID()

	work := t.TempDir()
	bundle := filepath.Join(work, "export")
	m, err := n.ExportContent(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Complete || len(m.Blobs) != 1 || len(m.Datasets) != 1 || len(m.Datasets[0].Versions) != 2 {
		t.Fatalf("export manifest: complete=%v blobs=%v datasets=%+v", m.Complete, m.Blobs, m.Datasets)
	}
	backup := filepath.Join(work, "authority.tar")
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	bm, err := n.BackupAuthority(f)
	f.Close()
	if err != nil || len(bm.Clusters) != 1 || len(bm.Clusters[0].Heads) == 0 {
		t.Fatalf("backup: %+v %v", bm, err)
	}

	// Wipe the machine.
	root := n.Repo.Root()
	n.Close()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	r, _, err := repo.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := os.Open(backup)
	rm, err := RestoreAuthority(r, rf)
	rf.Close()
	if err != nil || rm.Verified != len(bm.Clusters[0].Heads) {
		t.Fatalf("restore: %+v %v", rm, err)
	}
	n2, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if n2.PeerID() != peer {
		t.Fatal("restored node has another identity")
	}
	// Signed history is back; the content is not until the bundle is imported.
	if len(avail(t, n2, "photos")) != 2 {
		t.Fatal("restored state lost the version history")
	}
	if _, err := catString(t, n2, "photos@"+v1.TID+"/a.txt"); err == nil {
		t.Fatal("content readable before import")
	}
	res, err := n2.ImportContent(ctx, bundle)
	if err != nil || !res.Complete || res.Pins != len(m.Pins) || res.Blobs != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	vs := avail(t, n2, "photos")
	if vs[v1.TID].Availability != apiv0.AvailComplete || vs[v2.TID].Availability != apiv0.AvailComplete {
		t.Fatalf("restored versions: %+v", vs)
	}
	mustCat(t, n2, "photos@"+v1.TID+"/a.txt", "version one")
	mustCat(t, n2, "photos/a.txt", "version two")
	if got, err := catString(t, n2, "photos/big.bin"); err != nil || got != string(big) {
		t.Fatalf("big blob after restore: %d bytes, %v", len(got), err)
	}

	// A truncated bundle pins nothing.
	damaged := filepath.Join(work, "damaged")
	if err := os.MkdirAll(damaged, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ExportManifestFile, ExportCAR} {
		b, _ := os.ReadFile(filepath.Join(bundle, name))
		if name == ExportCAR {
			b = b[:len(b)/2]
		}
		os.WriteFile(filepath.Join(damaged, name), b, 0o600)
	}
	n3 := newLocal(t)
	if _, err := n3.ImportContent(ctx, damaged); err == nil {
		t.Fatal("truncated bundle imported")
	}
	if pins, _ := n3.Store.Pins(); len(pins) != 0 {
		t.Fatalf("damaged import pinned %d", len(pins))
	}
	// n3 has its own key: restoring another identity over it is refused.
	rf, _ = os.Open(backup)
	defer rf.Close()
	if _, err := RestoreAuthority(n3.Repo, rf); !isCode(err, apiv0.CodeConflict) {
		t.Fatalf("restore over another identity: %v", err)
	}
}
