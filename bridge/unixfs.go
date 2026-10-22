package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

type UnixFSType uint64

const (
	UnixFSRaw UnixFSType = iota
	UnixFSDirectory
	UnixFSFile
	UnixFSMetadata
	UnixFSSymlink
	UnixFSHAMTShard
)

type Link struct {
	CID   []byte
	Name  string
	Tsize uint64
}
type PBNode struct {
	Data  []byte
	Links []Link
}
type UnixFSData struct {
	Type             UnixFSType
	Data             []byte
	HasData          bool
	Filesize         uint64
	HasFilesize      bool
	Blocksizes       []uint64
	HashType, Fanout uint64
}

const maxPBField = 16 << 20

// DecodePBNode parses a dag-pb protobuf node. Unknown fields and duplicate singular fields
// are rejected to keep the accepted profile deterministic.
func DecodePBNode(b []byte) (PBNode, error) {
	var out PBNode
	seen := false
	for len(b) > 0 {
		f, w, v, rest, e := field(b)
		if e != nil {
			return out, e
		}
		b = rest
		switch f {
		case 1:
			if w != 2 || seen {
				return out, errors.New("dag-pb: invalid/duplicate Data")
			}
			seen = true
			out.Data = clone(v)
		case 2:
			if w != 2 {
				return out, errors.New("dag-pb: Link has wrong wire type")
			}
			l, e := decodePBLink(v)
			if e != nil {
				return out, e
			}
			out.Links = append(out.Links, l)
			if len(out.Links) > 1<<16 {
				return out, errors.New("dag-pb: too many links")
			}
		default:
			return out, fmt.Errorf("dag-pb: unknown field %d", f)
		}
	}
	return out, nil
}
func decodePBLink(b []byte) (Link, error) {
	var l Link
	seen := map[int]bool{}
	for len(b) > 0 {
		f, w, v, rest, e := field(b)
		if e != nil {
			return l, e
		}
		b = rest
		switch f {
		case 1:
			if w != 2 || seen[f] {
				return l, errors.New("dag-pb: invalid/duplicate Hash")
			}
			seen[f] = true
			l.CID = clone(v)
		case 2:
			if w != 2 || seen[f] {
				return l, errors.New("dag-pb: invalid/duplicate Name")
			}
			seen[f] = true
			l.Name = string(v)
		case 3:
			if w != 0 || seen[f] {
				return l, errors.New("dag-pb: invalid/duplicate Tsize")
			}
			seen[f] = true
			l.Tsize, _ = binary.Uvarint(v)
		default:
			return l, fmt.Errorf("dag-pb: unknown link field %d", f)
		}
	}
	if len(l.CID) == 0 {
		return l, errors.New("dag-pb: link missing Hash")
	}
	return l, nil
}
func DecodeUnixFSData(b []byte) (UnixFSData, error) {
	var d UnixFSData
	seen := map[int]bool{}
	haveType := false
	for len(b) > 0 {
		f, w, v, rest, e := field(b)
		if e != nil {
			return d, e
		}
		b = rest
		switch f {
		case 1:
			if w != 0 || seen[f] {
				return d, errors.New("unixfs: invalid Type")
			}
			seen[f] = true
			x, _ := binary.Uvarint(v)
			d.Type = UnixFSType(x)
			haveType = true
		case 2:
			if w != 2 || seen[f] {
				return d, errors.New("unixfs: invalid Data")
			}
			seen[f] = true
			d.Data = clone(v)
			d.HasData = true
		case 3:
			if w != 0 || seen[f] {
				return d, errors.New("unixfs: invalid filesize")
			}
			seen[f] = true
			d.Filesize, _ = binary.Uvarint(v)
			d.HasFilesize = true
		case 4:
			if w == 0 {
				x, _ := binary.Uvarint(v)
				d.Blocksizes = append(d.Blocksizes, x)
			} else if w == 2 {
				for len(v) > 0 {
					x, n := binary.Uvarint(v)
					if n <= 0 {
						return d, errors.New("unixfs: invalid packed blocksizes")
					}
					d.Blocksizes = append(d.Blocksizes, x)
					v = v[n:]
				}
			} else {
				return d, errors.New("unixfs: invalid blocksizes")
			}
		case 5:
			if w != 0 || seen[f] {
				return d, errors.New("unixfs: invalid hashType")
			}
			seen[f] = true
			d.HashType, _ = binary.Uvarint(v)
		case 6:
			if w != 0 || seen[f] {
				return d, errors.New("unixfs: invalid fanout")
			}
			seen[f] = true
			d.Fanout, _ = binary.Uvarint(v)
		default:
			return d, fmt.Errorf("unixfs: unknown field %d", f)
		}
	}
	if !haveType || d.Type > UnixFSHAMTShard {
		return d, errors.New("unixfs: missing or unsupported Type")
	}
	if len(d.Blocksizes) > 1<<16 {
		return d, errors.New("unixfs: too many blocksizes")
	}
	return d, nil
}
func field(b []byte) (int, int, []byte, []byte, error) {
	key, n := binary.Uvarint(b)
	if n <= 0 || key>>3 == 0 {
		return 0, 0, nil, nil, errors.New("protobuf: invalid field key")
	}
	f, w := int(key>>3), int(key&7)
	b = b[n:]
	switch w {
	case 0:
		_, n = binary.Uvarint(b)
		if n <= 0 {
			return 0, 0, nil, nil, errors.New("protobuf: invalid varint")
		}
		return f, w, b[:n], b[n:], nil
	case 2:
		z, n := binary.Uvarint(b)
		if n <= 0 || z > maxPBField || z > uint64(len(b)-n) {
			return 0, 0, nil, nil, errors.New("protobuf: invalid length")
		}
		return f, w, b[n : n+int(z)], b[n+int(z):], nil
	case 1:
		if len(b) < 8 {
			return 0, 0, nil, nil, errors.New("protobuf: truncated fixed64")
		}
		return f, w, b[:8], b[8:], nil
	case 5:
		if len(b) < 4 {
			return 0, 0, nil, nil, errors.New("protobuf: truncated fixed32")
		}
		return f, w, b[:4], b[4:], nil
	default:
		return 0, 0, nil, nil, errors.New("protobuf: unsupported wire type")
	}
}
func clone(b []byte) []byte { return append([]byte(nil), b...) }
