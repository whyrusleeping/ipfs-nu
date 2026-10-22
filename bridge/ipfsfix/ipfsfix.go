// Package ipfsfix builds small UnixFS DAGs and CARv1 archives for bridge tests, so kill,
// resume and pinset tests do not need Kubo. Encodings are canonical dag-pb (links before
// data) and UnixFS as Kubo writes them with raw leaves.
package ipfsfix

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"sort"

	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/p2p/host"
)

const (
	codecRaw   = 0x55
	codecDagPB = 0x70
)

// DAG is a set of verified-by-construction IPFS blocks.
type DAG struct {
	Blocks map[bridge.IPFSCID][]byte
	Order  []bridge.IPFSCID // insertion order (leaves first)
}

// New returns an empty DAG.
func New() *DAG { return &DAG{Blocks: map[bridge.IPFSCID][]byte{}} }

func (d *DAG) put(codec uint64, b []byte) bridge.IPFSCID {
	c := bridge.IPFSCID{Version: 1, Codec: codec, Hash: 0x12, Digest: sha256.Sum256(b)}
	if _, ok := d.Blocks[c]; !ok {
		d.Order = append(d.Order, c)
	}
	d.Blocks[c] = b
	return c
}

// Raw adds a raw block.
func (d *DAG) Raw(b []byte) bridge.IPFSCID { return d.put(codecRaw, b) }

// Link is a named dag-pb link.
type Link struct {
	Name  string
	CID   bridge.IPFSCID
	Tsize uint64
}

// Node adds a dag-pb node with UnixFS data unix.
func (d *DAG) Node(unix []byte, links []Link) bridge.IPFSCID {
	var b []byte
	for _, l := range links {
		var lb []byte
		lb = pbBytes(lb, 1, l.CID.Bytes())
		lb = pbBytes(lb, 2, []byte(l.Name))
		lb = pbVarint(lb, 3, l.Tsize)
		b = pbBytes(b, 2, lb)
	}
	b = pbBytes(b, 1, unix)
	return d.put(codecDagPB, b)
}

// File adds data chunked into raw leaves of chunk bytes; one chunk is a bare raw leaf.
func (d *DAG) File(data []byte, chunk int) bridge.IPFSCID {
	if len(data) <= chunk {
		return d.Raw(data)
	}
	var links []Link
	var sizes []uint64
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		links = append(links, Link{CID: d.Raw(data[off:end]), Tsize: uint64(end - off)})
		sizes = append(sizes, uint64(end-off))
	}
	unix := pbVarint(nil, 1, 2)
	unix = pbVarint(unix, 3, uint64(len(data)))
	for _, s := range sizes {
		unix = pbVarint(unix, 4, s)
	}
	return d.Node(unix, links)
}

// Symlink adds a UnixFS symlink node.
func (d *DAG) Symlink(target string) bridge.IPFSCID {
	return d.Node(pbBytes(pbVarint(nil, 1, 4), 2, []byte(target)), nil)
}

// Dir adds a UnixFS directory; links are sorted by name.
func (d *DAG) Dir(entries map[string]bridge.IPFSCID) bridge.IPFSCID {
	var links []Link
	for name, c := range entries {
		links = append(links, Link{Name: name, CID: c, Tsize: uint64(len(d.Blocks[c]))})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })
	return d.Node(pbVarint(nil, 1, 1), links)
}

// Files adds a directory of files chunked at chunk bytes.
func (d *DAG) Files(files map[string][]byte, chunk int) bridge.IPFSCID {
	entries := map[string]bridge.IPFSCID{}
	for name, data := range files {
		entries[name] = d.File(data, chunk)
	}
	return d.Dir(entries)
}

// Reachable lists the blocks reachable from root (root first), as a CAR would carry them.
func (d *DAG) Reachable(root bridge.IPFSCID) []bridge.IPFSCID {
	seen := map[bridge.IPFSCID]bool{}
	var out []bridge.IPFSCID
	var walk func(bridge.IPFSCID)
	walk = func(c bridge.IPFSCID) {
		if seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
		if c.Codec != codecDagPB {
			return
		}
		pb, err := bridge.DecodePBNode(d.Blocks[c])
		if err != nil {
			return
		}
		for _, l := range pb.Links {
			if lc, err := bridge.ParseIPFSBytes(l.CID); err == nil {
				walk(lc)
			}
		}
	}
	walk(root)
	return out
}

// CAR encodes a CARv1 with the given root and the blocks reachable from it.
func (d *DAG) CAR(root bridge.IPFSCID) []byte {
	cb := append([]byte{0}, root.Bytes()...)
	hdr := []byte{0xa2, 0x65, 'r', 'o', 'o', 't', 's', 0x81, 0xd8, 0x2a, 0x58, byte(len(cb))}
	hdr = append(hdr, cb...)
	hdr = append(hdr, 0x67, 'v', 'e', 'r', 's', 'i', 'o', 'n', 1)
	var out bytes.Buffer
	out.Write(binary.AppendUvarint(nil, uint64(len(hdr))))
	out.Write(hdr)
	for _, c := range d.Reachable(root) {
		sec := append(c.Bytes(), d.Blocks[c]...)
		out.Write(binary.AppendUvarint(nil, uint64(len(sec))))
		out.Write(sec)
	}
	return out.Bytes()
}

// ServeBitswap answers /ipfs/bitswap/1.2.0 wants on h from d, the way Kubo does: each want
// arrives on its own stream and the block is sent back on a new stream. serve, if set, is
// asked first and may block or refuse (false) a want.
func (d *DAG) ServeBitswap(h *host.Host, serve func(bridge.IPFSCID) bool) error {
	return h.Handle(bridge.BitswapProtocol, 64, func(st *host.Stream) {
		remote := st.Conn().PeerID()
		r := bufio.NewReader(st)
		n, err := binary.ReadUvarint(r)
		if err != nil || n > 1<<20 {
			_ = st.Close()
			return
		}
		msg := make([]byte, n)
		_, err = io.ReadFull(r, msg)
		_ = st.Close()
		if err != nil {
			return
		}
		for _, c := range wants(msg) {
			b, ok := d.Blocks[c]
			if !ok || (serve != nil && !serve(c)) {
				continue
			}
			prefix := binary.AppendUvarint(nil, c.Version)
			prefix = binary.AppendUvarint(prefix, c.Codec)
			prefix = binary.AppendUvarint(prefix, 0x12)
			prefix = binary.AppendUvarint(prefix, 32) // CIDv0 is [0, dag-pb, sha2-256, 32], as Kubo sends
			payload := pbBytes(pbBytes(nil, 1, prefix), 2, b)
			reply, err := h.OpenStream(context.Background(), remote, bridge.BitswapProtocol)
			if err != nil {
				return
			}
			out := pbBytes(nil, 3, payload)
			_, _ = reply.Write(append(binary.AppendUvarint(nil, uint64(len(out))), out...))
			_ = reply.Close()
		}
	})
}

// wants extracts the CIDs of a Bitswap wantlist message.
func wants(msg []byte) []bridge.IPFSCID {
	var out []bridge.IPFSCID
	for _, wl := range fields(msg, 1) {
		for _, entry := range fields(wl, 1) {
			for _, blk := range fields(entry, 1) {
				if c, err := bridge.ParseIPFSBytes(blk); err == nil {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// fields returns the length-delimited values of field f (other fields skipped).
func fields(b []byte, f uint64) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		switch key & 7 {
		case 0:
			_, n = binary.Uvarint(b)
			if n <= 0 {
				return out
			}
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return out
			}
			if key>>3 == f {
				out = append(out, b[n:n+int(l)])
			}
			b = b[n+int(l):]
		default:
			return out
		}
	}
	return out
}

func pbVarint(b []byte, field int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(field<<3))
	return binary.AppendUvarint(b, v)
}

func pbBytes(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field<<3|2))
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}
