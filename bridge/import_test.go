package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"io"
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
	root := add(codecDagPB, dirPB)
	st := &sink{MemStore: mst.MemStore{}, blobs: map[cid.CID][]byte{}}
	result, err := Import(context.Background(), root, blocks, st, st, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || !result.Manifest.Defined() {
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
}
