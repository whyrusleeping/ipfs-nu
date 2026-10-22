package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/node/repo"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIPFSImportRecoveryKeepsOperationID(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "nu")
	r, _, err := repo.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("resume after restart")
	sum := sha256.Sum256(data)
	leaf := bridge.IPFSCID{Version: 1, Codec: 0x55, Hash: 0x12, Digest: sum}
	unix := []byte{0x08, 0x01}
	link := []byte{0x0a, byte(len(leaf.Bytes()))}
	link = append(link, leaf.Bytes()...)
	link = append(link, 0x12, 1, 'x')
	rootData := append([]byte{0x0a, byte(len(unix))}, unix...)
	rootData = append(rootData, 0x12, byte(len(link)))
	rootData = append(rootData, link...)
	rootSum := sha256.Sum256(rootData)
	root := bridge.IPFSCID{Version: 1, Codec: 0x70, Hash: 0x12, Digest: rootSum}
	car := filepath.Join(t.TempDir(), "root.car")
	if err := os.WriteFile(car, makeCAR(root, rootData, leaf, data), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	before, err := n.Store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	dry, err := n.ImportIPFS(context.Background(), apiv0.IPFSImportRequest{Root: root.String(), CAR: car, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	dryFinal, err := n.WaitOperation(context.Background(), dry.ID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var plan apiv0.IPFSImportResult
	if err = json.Unmarshal(dryFinal.Result, &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Files != 1 || plan.Bytes != uint64(len(data)) || plan.EstimatedPhysicalBytes < uint64(len(data)) {
		t.Fatalf("preflight plan: %+v", plan)
	}
	after, err := n.Store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if before.Objects != after.Objects || before.Pins != after.Pins {
		t.Fatal("dry-run changed the store")
	}
	operation, err := n.Ops.StartWithID("durablefixture01", "import-ipfs", root.String())
	if err != nil {
		t.Fatal(err)
	}
	job := ipfsImportJob{Request: apiv0.IPFSImportRequest{Root: root.String(), CAR: car}, Operation: operation.Snapshot()}
	if err = n.saveIPFSJob(job); err != nil {
		t.Fatal(err)
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := recovered.WaitOperation(ctx, "durablefixture01", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != apiv0.StateSucceeded {
		t.Fatalf("operation: %+v", final)
	}
	var out struct {
		IPFSRoot string `json:"ipfs_root"`
		Root     string `json:"root"`
	}
	if err := json.Unmarshal(final.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.IPFSRoot != root.String() || out.Root == "" {
		t.Fatalf("result: %+v", out)
	}
}
func makeCAR(root bridge.IPFSCID, rootData []byte, leaf bridge.IPFSCID, leafData []byte) []byte {
	hdr := []byte{0xa2, 0x65, 'r', 'o', 'o', 't', 's', 0x81, 0xd8, 0x2a, 0x58, 0x25, 0}
	hdr = append(hdr, root.Bytes()...)
	hdr = append(hdr, 0x67, 'v', 'e', 'r', 's', 'i', 'o', 'n', 1)
	var out bytes.Buffer
	putUvarint(&out, uint64(len(hdr)))
	out.Write(hdr)
	for _, b := range []struct {
		c    bridge.IPFSCID
		data []byte
	}{{root, rootData}, {leaf, leafData}} {
		section := append(b.c.Bytes(), b.data...)
		putUvarint(&out, uint64(len(section)))
		out.Write(section)
	}
	return out.Bytes()
}
func putUvarint(w *bytes.Buffer, n uint64) {
	var b [10]byte
	i := binary.PutUvarint(b[:], n)
	w.Write(b[:i])
}
