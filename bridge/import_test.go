package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type memBlocks map[IPFSCID][]byte

func (m memBlocks) GetBlock(_ context.Context, c IPFSCID) ([]byte, error) {
	b, ok := m[c]
	if !ok {
		return nil, ErrBlockNotFound
	}
	return b, nil
}

var ErrBlockNotFound = errNotFound("fixture block absent")

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

type sink struct {
	mst.MemStore
	blobs map[cid.CID][]byte
}

func (s *sink) AddBlob(r io.Reader, size int64) (cid.CID, error) {
	b, e := io.ReadAll(r)
	if e != nil {
		return cid.CID{}, e
	}
	if size >= 0 && int64(len(b)) != size {
		return cid.CID{}, io.ErrUnexpectedEOF
	}
	c, e := cid.SumCID(cid.Raw, cid.BLAKE3, b)
	if e == nil {
		s.blobs[c] = b
	}
	return c, e
}
func TestCheckpointSourceSurvivesRestartAndVerifies(t *testing.T) {
	data := []byte("checkpoint")
	digest := sha256.Sum256(data)
	id := IPFSCID{Version: 1, Codec: codecRaw, Hash: hashSHA256, Digest: digest}
	dir := t.TempDir()
	upstream := memBlocks{id: data}
	first := CheckpointSource{Dir: dir, Upstream: upstream}
	if got, err := first.GetBlock(context.Background(), id); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("first fetch: %q %v", got, err)
	}
	second := CheckpointSource{Dir: dir} // restarted job has no need to reread/refetch this block
	if got, err := second.GetBlock(context.Background(), id); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("resume: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointFilename(id)), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := second.GetBlock(context.Background(), id); err == nil {
		t.Fatal("corrupt cache accepted without upstream")
	}
}

func checkpointFilename(id IPFSCID) string {
	key := sha256.Sum256(id.Bytes())
	return hex.EncodeToString(key[:]) + ".block"
}

func TestImportDirectoryFileAndSymlink(t *testing.T) {
	blocks := memBlocks{}
	add := func(codec uint64, b []byte) IPFSCID {
		d := sha256.Sum256(b)
		c := IPFSCID{Version: 1, Codec: codec, Hash: hashSHA256, Digest: d}
		blocks[c] = b
		return c
	}
	file := add(codecRaw, []byte("payload"))
	symData := []byte{0x08, 0x04, 0x12, 0x06, 't', 'a', 'r', 'g', 'e', 't'}
	symPB := []byte{0x0a, byte(len(symData))}
	symPB = append(symPB, symData...)
	sym := add(codecDagPB, symPB)
	execLeaf := add(codecRaw, []byte("exec"))
	execData := []byte{0x08, 0x02, 0x18, 0x04, 0x38, 0xed, 0x03} // UnixFS File, 4 bytes, mode 0755
	execLink := []byte{0x0a, byte(len(execLeaf.Bytes()))}
	execLink = append(execLink, execLeaf.Bytes()...)
	execPB := []byte{0x0a, byte(len(execData))}
	execPB = append(execPB, execData...)
	execPB = append(execPB, 0x12, byte(len(execLink)))
	execPB = append(execPB, execLink...)
	execFile := add(codecDagPB, execPB)
	dirData := []byte{0x08, 0x01}
	encodeLink := func(c IPFSCID, name string) []byte {
		l := []byte{0x0a, byte(len(c.Bytes()))}
		l = append(l, c.Bytes()...)
		l = append(l, 0x12, byte(len(name)))
		l = append(l, []byte(name)...)
		return append([]byte{0x12, byte(len(l))}, l...)
	}
	dirPB := append([]byte{0x0a, byte(len(dirData))}, dirData...)
	dirPB = append(dirPB, encodeLink(file, "f")...)
	dirPB = append(dirPB, encodeLink(sym, "s")...)
	dirPB = append(dirPB, encodeLink(execFile, "x")...)
	root := add(codecDagPB, dirPB)
	st := &sink{MemStore: mst.MemStore{}, blobs: map[cid.CID][]byte{}}
	delete(blocks, sym) // simulate interruption after the file block was verified/stored
	if _, err := Import(context.Background(), root, blocks, st, st, ImportOptions{}); err == nil {
		t.Fatal("incomplete source unexpectedly completed")
	}
	blocks[sym] = symPB
	result, err := Import(context.Background(), root, blocks, st, st, ImportOptions{}) // retry reuses deterministic stored outputs
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 2 || !result.Manifest.Defined() {
		t.Fatalf("result: %+v", result)
	}
	headerBytes, err := st.Get(result.Root)
	if err != nil {
		t.Fatal(err)
	}
	h, err := tree.DecodeHeader(headerBytes)
	if err != nil {
		t.Fatal(err)
	}
	entries := tree.Dir(st, h.Root)
	f, ok, err := entries.Get("f")
	if err != nil || !ok || f.Kind != tree.KindFile {
		t.Fatalf("file entry %+v %v %v", f, ok, err)
	}
	x, ok, err := entries.Get("x")
	if err != nil || !ok || x.Kind != tree.KindFile || x.Mode != tree.ModeExec {
		t.Fatalf("UnixFS executable mode not preserved: %+v %v %v", x, ok, err)
	}
	s, ok, err := entries.Get("s")
	if err != nil || !ok || s.Kind != tree.KindSymlink {
		t.Fatalf("symlink entry %+v %v %v", s, ok, err)
	}
	if !bytes.Equal(st.blobs[f.CID], []byte("payload")) || !bytes.Equal(st.blobs[s.CID], []byte("target")) {
		t.Fatal("converted bytes differ")
	}
	manifest, err := st.Get(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = drisl.Decode(manifest); err != nil {
		t.Fatal(err)
	}
	fileResult, err := Import(context.Background(), file, blocks, st, st, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fileNodeData, err := st.Get(fileResult.Root)
	if err != nil {
		t.Fatal(err)
	}
	fileNode, err := tree.DecodeFileNode(fileNodeData)
	if err != nil {
		t.Fatal(err)
	}
	if fileNode.Src != f.CID {
		t.Fatalf("standalone file root does not wrap imported blob: %+v", fileNode)
	}
}
