// Package disco implements Nu-only authenticated path discovery over an admitted
// inner QUIC connection. It never discovers peers or authorizes public access.
package disco

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net/netip"
	"time"

	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const Protocol = "/nu/path/1"
const MaxCandidates = 16
const probeSize = 96
const maxControl = 11 + MaxCandidates*22

type Kind uint8

const (
	LAN Kind = iota + 1
	IPv6
	Mapped
	Reflexive
)

// Candidate is a tentative, exact endpoint, never a validated packet path.
// Expires is mandatory; no candidate lease may exceed ten minutes on the wire.
type Candidate struct {
	Kind    Kind
	Addr    netip.AddrPort
	Expires time.Time
}

func validEndpoint(a netip.AddrPort) bool {
	ip := a.Addr()
	return a.IsValid() && a.Port() != 0 && ip.Zone() == "" && !ip.Is4In6() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() && !magicsock.IsVirtual(a) && ip != netip.MustParseAddr("255.255.255.255")
}
func putEndpoint(b []byte, a netip.AddrPort) {
	clear(b[:19])
	if a.Addr().Is4() {
		b[0] = 4
		v := a.Addr().As4()
		copy(b[13:17], v[:])
	} else {
		b[0] = 6
		v := a.Addr().As16()
		copy(b[1:17], v[:])
	}
	binary.BigEndian.PutUint16(b[17:19], a.Port())
}
func getEndpoint(b []byte) (netip.AddrPort, error) {
	var ip netip.Addr
	switch b[0] {
	case 4:
		if !bytes.Equal(b[1:13], make([]byte, 12)) {
			return netip.AddrPort{}, wire.ErrFormat
		}
		ip = netip.AddrFrom4([4]byte(b[13:17]))
	case 6:
		ip = netip.AddrFrom16([16]byte(b[1:17]))
	default:
		return netip.AddrPort{}, wire.ErrFormat
	}
	a := netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[17:19]))
	if !validEndpoint(a) {
		return netip.AddrPort{}, wire.ErrFormat
	}
	return a, nil
}
func encodeControl(gen uint64, list []Candidate, now time.Time) ([]byte, error) {
	if gen == 0 || len(list) > MaxCandidates {
		return nil, wire.ErrLimit
	}
	b := make([]byte, 11+22*len(list))
	b[0] = 1
	b[1] = 1
	binary.BigEndian.PutUint64(b[2:10], gen)
	b[10] = byte(len(list))
	seen := make(map[netip.AddrPort]bool)
	for i, c := range list {
		ttl := int(c.Expires.Sub(now) / time.Second)
		if c.Kind < LAN || c.Kind > Reflexive || !validEndpoint(c.Addr) || ttl < 1 || seen[c.Addr] {
			return nil, wire.ErrFormat
		}
		if ttl > 600 {
			ttl = 600
		}
		seen[c.Addr] = true
		off := 11 + 22*i
		b[off] = byte(c.Kind)
		binary.BigEndian.PutUint16(b[off+1:off+3], uint16(ttl))
		putEndpoint(b[off+3:off+22], c.Addr)
	}
	return b, nil
}
func decodeControl(b []byte, now time.Time) (uint64, []Candidate, error) {
	if len(b) < 11 || len(b) > maxControl || b[0] != 1 || b[1] != 1 || int(b[10]) > MaxCandidates || len(b) != 11+22*int(b[10]) {
		return 0, nil, wire.ErrFormat
	}
	gen := binary.BigEndian.Uint64(b[2:10])
	if gen == 0 {
		return 0, nil, wire.ErrFormat
	}
	list := make([]Candidate, 0, int(b[10]))
	seen := make(map[netip.AddrPort]bool)
	for off := 11; off < len(b); off += 22 {
		a, err := getEndpoint(b[off+3 : off+22])
		kind := Kind(b[off])
		ttl := binary.BigEndian.Uint16(b[off+1 : off+3])
		if err != nil || kind < LAN || kind > Reflexive || ttl < 1 || ttl > 600 || seen[a] {
			return 0, nil, wire.ErrFormat
		}
		seen[a] = true
		list = append(list, Candidate{kind, a, now.Add(time.Duration(ttl) * time.Second)})
	}
	return gen, list, nil
}
func readControl(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if n < 11 || n > maxControl {
		return nil, wire.ErrLimit
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func writeControl(w io.Writer, b []byte) error {
	var h [2]byte
	binary.BigEndian.PutUint16(h[:], uint16(len(b)))
	// io.Copy handles short writes, including hostile test readers/writers.
	_, err := io.Copy(w, io.MultiReader(bytes.NewReader(h[:]), bytes.NewReader(b)))
	return err
}

type probe struct {
	kind      byte
	session   [16]byte
	seq       uint64
	challenge [16]byte
	observed  netip.AddrPort
}

func encodeProbe(p probe, key []byte) []byte {
	b := make([]byte, probeSize)
	b[0] = 1
	b[1] = p.kind
	copy(b[2:18], p.session[:])
	binary.BigEndian.PutUint64(b[18:26], p.seq)
	copy(b[26:42], p.challenge[:])
	if p.kind == 2 {
		putEndpoint(b[42:61], p.observed)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b[:64])
	copy(b[64:], mac.Sum(nil))
	return b
}
func decodeProbe(b, key []byte) (probe, error) {
	var p probe
	if len(b) != probeSize || b[0] != 1 || (b[1] != 1 && b[1] != 2) || !bytes.Equal(b[61:64], []byte{0, 0, 0}) {
		return p, wire.ErrFormat
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b[:64])
	if !hmac.Equal(mac.Sum(nil), b[64:]) {
		return p, wire.ErrFormat
	}
	p.kind = b[1]
	copy(p.session[:], b[2:18])
	p.seq = binary.BigEndian.Uint64(b[18:26])
	copy(p.challenge[:], b[26:42])
	if p.seq == 0 {
		return probe{}, wire.ErrFormat
	}
	if p.kind == 1 {
		if !bytes.Equal(b[42:61], make([]byte, 19)) {
			return probe{}, wire.ErrFormat
		}
	} else {
		var err error
		p.observed, err = getEndpoint(b[42:61])
		if err != nil {
			return probe{}, err
		}
	}
	return p, nil
}

// replayWindow accepts reorderings within 64 packets, once each. It is updated
// only after source, format and MAC validation, never by unauthenticated input.
type replayWindow struct{ top, bits uint64 }

func (w *replayWindow) accept(n uint64) bool {
	if n == 0 {
		return false
	}
	if n > w.top {
		delta := n - w.top
		if delta >= 64 {
			w.bits = 0
		} else {
			w.bits <<= delta
		}
		w.bits |= 1
		w.top = n
		return true
	}
	d := w.top - n
	if d >= 64 || w.bits&(uint64(1)<<d) != 0 {
		return false
	}
	w.bits |= uint64(1) << d
	return true
}

type exporter interface {
	ExportKeyingMaterial(string, []byte, int) ([]byte, error)
}

func keys(c exporter, local, remote wire.PeerID, localNonce, remoteNonce [16]byte) (id [16]byte, tx, rx []byte, err error) {
	a, b := local.Bytes(), remote.Bytes()
	low := bytes.Compare(a, b) < 0
	if !low {
		a, b = b, a
		localNonce, remoteNonce = remoteNonce, localNonce
	}
	ctx := make([]byte, 4+len(a)+len(b)+32)
	binary.BigEndian.PutUint16(ctx[:2], uint16(len(a)))
	copy(ctx[2:], a)
	off := 2 + len(a)
	binary.BigEndian.PutUint16(ctx[off:off+2], uint16(len(b)))
	copy(ctx[off+2:], b)
	off += 2 + len(b)
	copy(ctx[off:off+16], localNonce[:])
	copy(ctx[off+16:], remoteNonce[:])
	out, err := c.ExportKeyingMaterial("EXPORTER-nu-path-1", ctx, 80)
	if err != nil {
		return id, nil, nil, err
	}
	copy(id[:], out[:16])
	tx, rx = out[16:48], out[48:80]
	if !low {
		tx, rx = rx, tx
	}
	return id, tx, rx, nil
}
