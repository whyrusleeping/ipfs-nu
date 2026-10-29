package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/swarm"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

func serve(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fl.String("listen", "/ip4/0.0.0.0/udp/4181/quic-v1", "swarm listen address")
	addr := fl.String("addr", "", "address to put in the link (default: the advertised listen addresses)")
	files := fl.Int("files", 1, "number of files (more than one: directories of 100)")
	sizeFlag := fl.String("size", "4GiB", "size of each file (KiB/MiB/GiB suffixes)")
	seed := fl.Uint64("seed", 1, "content seed; servers with the same flags hold the same CIDs")
	root := fl.String("store", "", "directory for the store (default tmpfs when present)")
	linkFile := fl.String("link-file", "", "also write the link to this file")
	fl.Parse(args)
	size, err := parseSize(*sizeFlag)
	if err != nil {
		return err
	}
	if *files < 1 {
		return fmt.Errorf("-files must be at least 1")
	}
	dir, err := tempStoreDir(*root, "xferbench-serve-")
	if err != nil {
		return err
	}
	defer removeAll(dir)
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()

	start := time.Now()
	built, err := tree.BuildFS(synthFS{n: *files, size: size, seed: *seed}, ".", tree.BuildOptions{Blobs: st, Nodes: st})
	if err != nil {
		return fmt.Errorf("generating content: %w", err)
	}
	gen := time.Since(start)

	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	sw, err := swarm.Start(ctx, swarm.Config{Key: key, Store: st, Listen: []string{*listen},
		Logger: slog.New(slog.DiscardHandler), DHT: swarm.DHTConfig{Mode: "off"}})
	if err != nil {
		return err
	}
	defer sw.Close()
	addrs := sw.Addrs()
	if *addr != "" {
		a, err := wire.ParseMultiaddr(*addr, 1024)
		if err != nil {
			return err
		}
		addrs = []wire.Multiaddr{a}
	}
	l := (&link.Link{CID: built.Header, Providers: []link.Provider{{Peer: sw.ID(), Addrs: addrs}}}).String()
	if *linkFile != "" {
		if err := os.WriteFile(*linkFile, []byte(l+"\n"), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("serving %d files, %d bytes (generated in %.1fs) as %s\nlink: %s\n", built.Files, built.Bytes, gen.Seconds(), built.Header, l)
	cpu := startCPU()
	<-ctx.Done()
	fmt.Printf("served for %.1fs using %.2f cores on average\n", time.Since(cpu.wall).Seconds(), cpu.cores())
	return nil
}

// synthFS is a read-only file system of n files of size bytes each, with deterministic
// pseudo-random contents (ChaCha8 keyed by the seed and the file's index). One file is
// data.bin at the root; more are d000/f00000.bin, d000/f00001.bin, … 100 per directory.
type synthFS struct {
	n    int
	size int64
	seed uint64
}

const perDir = 100

func (s synthFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if ents, ok := s.dir(name); ok {
		return &synthDir{info: dirInfo(path.Base(name)), ents: ents}, nil
	}
	if i, ok := s.file(name); ok {
		var key [32]byte
		binary.LittleEndian.PutUint64(key[:], s.seed)
		binary.LittleEndian.PutUint64(key[8:], uint64(i))
		return &synthFile{info: fileInfo{name: path.Base(name), size: s.size}, r: io.LimitReader(rand.NewChaCha8(key), s.size)}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS.
func (s synthFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if ents, ok := s.dir(name); ok {
		return ents, nil
	}
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
}

func (s synthFS) dir(name string) ([]fs.DirEntry, bool) {
	var ents []fs.DirEntry
	switch {
	case name == "." && s.n == 1:
		ents = append(ents, fs.FileInfoToDirEntry(fileInfo{name: "data.bin", size: s.size}))
	case name == ".":
		for d := 0; d*perDir < s.n; d++ {
			ents = append(ents, fs.FileInfoToDirEntry(dirInfo(fmt.Sprintf("d%03d", d))))
		}
	case s.n > 1 && strings.HasPrefix(name, "d") && !strings.Contains(name, "/"):
		var d int
		if _, err := fmt.Sscanf(name, "d%03d", &d); err != nil || fmt.Sprintf("d%03d", d) != name || d*perDir >= s.n {
			return nil, false
		}
		for i := d * perDir; i < min((d+1)*perDir, s.n); i++ {
			ents = append(ents, fs.FileInfoToDirEntry(fileInfo{name: fmt.Sprintf("f%05d.bin", i), size: s.size}))
		}
	default:
		return nil, false
	}
	return ents, true
}

func (s synthFS) file(name string) (int, bool) {
	if s.n == 1 {
		return 0, name == "data.bin"
	}
	var d, i int
	if _, err := fmt.Sscanf(name, "d%03d/f%05d.bin", &d, &i); err != nil || fmt.Sprintf("d%03d/f%05d.bin", d, i) != name {
		return 0, false
	}
	return i, i/perDir == d && i < s.n
}

type fileInfo struct {
	name string
	size int64
	dir  bool
}

func dirInfo(name string) fileInfo { return fileInfo{name: name, dir: true} }

func (f fileInfo) Name() string { return f.name }
func (f fileInfo) Size() int64  { return f.size }
func (f fileInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (f fileInfo) ModTime() time.Time { return time.Time{} }
func (f fileInfo) IsDir() bool        { return f.dir }
func (f fileInfo) Sys() any           { return nil }

type synthFile struct {
	info fileInfo
	r    io.Reader
}

func (f *synthFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *synthFile) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *synthFile) Close() error               { return nil }

type synthDir struct {
	info fileInfo
	ents []fs.DirEntry
}

func (d *synthDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *synthDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}
func (d *synthDir) Close() error { return nil }
func (d *synthDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		ents := d.ents
		d.ents = nil
		return ents, nil
	}
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	k := min(n, len(d.ents))
	ents := d.ents[:k]
	d.ents = d.ents[k:]
	return ents, nil
}
