package bridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const BitswapProtocol = "/ipfs/bitswap/1.2.0"
const (
	maxBitswapBlock   = 2 << 20
	maxBitswapMessage = (2 << 20) + 64<<10
)

// ProviderFinder exposes Amino provider discovery without coupling the importer to node
// assembly. The native multihash bytes, not a SHA-256 alias, are the routing key.
type ProviderFinder interface {
	FindProviders(context.Context, []byte) ([]kad.Peer, error)
}
type bitswapReply struct {
	data []byte
	err  error
}
type BitswapSource struct {
	Host      *host.Host
	Providers ProviderFinder
	Timeout   time.Duration
	mu        sync.Mutex
	pending   map[IPFSCID][]chan bitswapReply
	handler   bool
}

func NewBitswapSource(h *host.Host, p ProviderFinder) *BitswapSource {
	return &BitswapSource{Host: h, Providers: p, Timeout: 20 * time.Second, pending: map[IPFSCID][]chan bitswapReply{}}
}
func (s *BitswapSource) ensureHandler() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handler {
		return nil
	}
	if s.Host == nil {
		return errors.New("bitswap: host required")
	}
	if err := s.Host.Handle(BitswapProtocol, 64, s.receive); err != nil {
		return err
	}
	s.handler = true
	return nil
}
func (s *BitswapSource) Close() {
	s.mu.Lock()
	registered := s.handler
	s.handler = false
	s.mu.Unlock()
	if registered && s.Host != nil {
		s.Host.RemoveHandler(BitswapProtocol)
	}
}
func (s *BitswapSource) receive(st *host.Stream) {
	defer st.Close()
	r := bufio.NewReader(st)
	for {
		msg, e := readDelimited(r, maxBitswapMessage)
		if e != nil {
			return
		}
		for _, dont := range parseDontHaves(msg) {
			s.deliver(dont, bitswapReply{err: &MissingBlockError{CID: dont, Err: fmt.Errorf("provider %s answered DONT_HAVE", st.Conn().PeerID())}})
		}
		data, id, e := parseBitswapPayload(msg)
		s.mu.Lock()
		chs := append([]chan bitswapReply(nil), s.pending[id]...)
		s.mu.Unlock()
		if id.Hash != 0 {
			reply := bitswapReply{data: data, err: e}
			for _, ch := range chs {
				select {
				case ch <- reply:
				default:
				}
			}
		}
	}
}
func (s *BitswapSource) deliver(id IPFSCID, reply bitswapReply) {
	s.mu.Lock()
	chs := append([]chan bitswapReply(nil), s.pending[id]...)
	s.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- reply:
		default:
		}
	}
}

// parseDontHaves returns the CIDs of a message's DONT_HAVE block presences (Bitswap 1.2
// field 4: {cid = 1, type = 2; DontHave = 1}).
func parseDontHaves(b []byte) []IPFSCID {
	var out []IPFSCID
	for len(b) > 0 {
		f, w, v, rest, e := pbField(b)
		if e != nil {
			return out
		}
		b = rest
		if f != 4 || w != 2 {
			continue
		}
		var id IPFSCID
		var ok, dont bool
		for len(v) > 0 {
			ff, ww, vv, rr, e := pbField(v)
			if e != nil {
				break
			}
			v = rr
			switch {
			case ff == 1 && ww == 2:
				id, e = ParseIPFSBytes(vv)
				ok = e == nil
			case ff == 2 && ww == 0:
				t, _ := binary.Uvarint(vv)
				dont = t == 1
			}
		}
		if ok && dont {
			out = append(out, id)
		}
	}
	return out
}

func (s *BitswapSource) GetBlock(ctx context.Context, id IPFSCID) ([]byte, error) {
	if s.Providers == nil {
		return nil, errors.New("bitswap: provider finder required")
	}
	if e := s.ensureHandler(); e != nil {
		return nil, e
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	replyCh := make(chan bitswapReply, 1)
	s.mu.Lock()
	s.pending[id] = append(s.pending[id], replyCh)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		list := s.pending[id]
		for i, ch := range list {
			if ch == replyCh {
				s.pending[id] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(s.pending[id]) == 0 {
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}()
	ps, e := s.Providers.FindProviders(ctx, append([]byte{0x12, 32}, id.Digest[:]...))
	if e != nil {
		return nil, e
	}
	var errs []string
	for _, p := range ps {
		if s.Host == nil {
			return nil, errors.New("bitswap: host required")
		}
		if p.ID == s.Host.ID() {
			continue
		}
		conn, ok := s.Host.Connection(p.ID)
		var dialErr error
		if !ok {
			for _, raw := range p.Addrs {
				a, er := wire.MultiaddrFromBytes(raw, 2048)
				if er != nil || a.Opaque() {
					continue
				}
				text, er := a.Text()
				if er != nil {
					continue
				}
				if !strings.Contains(text, "/p2p/") {
					text += "/p2p/" + p.ID.String()
				}
				ma, er := wire.ParseMultiaddr(text, 4096)
				if er != nil {
					continue
				}
				conn, er = s.Host.Dial(ctx, ma)
				dialErr = er
				if er == nil {
					break
				}
			}
		}
		if conn == nil {
			if dialErr != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", p.ID, dialErr))
			} else {
				errs = append(errs, fmt.Sprintf("%s: no usable address", p.ID))
			}
			continue
		}
		st, er := s.Host.OpenStream(ctx, p.ID, BitswapProtocol)
		if er != nil {
			errs = append(errs, er.Error())
			continue
		}
		if er = bitswapSend(st, id); er != nil {
			errs = append(errs, er.Error())
			continue
		}
		break
	}
	if len(errs) > 0 {
		select {
		case reply := <-replyCh:
			if reply.err != nil {
				return nil, reply.err
			}
			if e = id.Verify(reply.data); e != nil {
				return nil, e
			}
			return reply.data, nil
		default:
		}
	}
	select {
	case reply := <-replyCh:
		if reply.err != nil {
			return nil, reply.err
		}
		if e = id.Verify(reply.data); e != nil {
			return nil, e
		}
		return reply.data, nil
	case <-ctx.Done():
		if len(errs) > 0 {
			return nil, fmt.Errorf("bitswap: all providers failed: %s; %w", strings.Join(errs, "; "), ctx.Err())
		}
		return nil, ctx.Err()
	}
}
func bitswapSend(st *host.Stream, id IPFSCID) error {
	defer st.Close()
	entry := pbBytes(nil, 1, id.Bytes())
	entry = pbVarint(entry, 2, 1)
	entry = pbVarint(entry, 4, 0)
	entry = pbVarint(entry, 5, 1)
	want := pbBytes(nil, 1, entry)
	want = pbVarint(want, 2, 1)
	msg := pbBytes(nil, 1, want)
	return writeDelimited(st, msg)
}
func parseBitswapPayload(b []byte) ([]byte, IPFSCID, error) {
	for len(b) > 0 {
		f, w, v, rest, e := pbField(b)
		if e != nil {
			return nil, IPFSCID{}, e
		}
		b = rest
		if f != 3 || w != 2 {
			continue
		}
		var prefix, data, cidBytes []byte
		bb := v
		for len(bb) > 0 {
			ff, ww, vv, rr, e := pbField(bb)
			if e != nil {
				return nil, IPFSCID{}, e
			}
			bb = rr
			if ww != 2 {
				continue
			}
			switch ff {
			case 1:
				prefix = vv
			case 2:
				data = vv
			case 3:
				cidBytes = vv
			}
		}
		var id IPFSCID
		if len(cidBytes) > 0 {
			id, e = ParseIPFSBytes(cidBytes)
		} else if len(prefix) > 0 {
			id, e = cidFromPrefix(prefix, data)
		} else {
			return nil, IPFSCID{}, errors.New("bitswap: payload missing CID/prefix")
		}
		if e != nil {
			return nil, IPFSCID{}, e
		}
		if e = id.Verify(data); e != nil {
			return nil, IPFSCID{}, e
		}
		if len(data) > maxBitswapBlock {
			return nil, IPFSCID{}, errors.New("bitswap: block exceeds 2 MiB")
		}
		return append([]byte(nil), data...), id, nil
	}
	return nil, IPFSCID{}, errors.New("bitswap: response contains no block payload")
}
func cidFromPrefix(p, data []byte) (IPFSCID, error) {
	v, n := binary.Uvarint(p)
	if n <= 0 {
		return IPFSCID{}, errors.New("bitswap: malformed CID prefix")
	}
	p = p[n:]
	codec, n := binary.Uvarint(p)
	if n <= 0 {
		return IPFSCID{}, errors.New("bitswap: malformed CID prefix")
	}
	p = p[n:]
	h, n := binary.Uvarint(p)
	if n <= 0 {
		return IPFSCID{}, errors.New("bitswap: malformed CID prefix")
	}
	p = p[n:]
	sz, n := binary.Uvarint(p)
	if n <= 0 || n != len(p) || sz != 32 || h != hashSHA256 {
		return IPFSCID{}, errors.New("bitswap: unsupported CID prefix")
	}
	// Kubo sends CIDv0 blocks with the prefix [0, dag-pb, sha2-256, 32].
	if (v != 1 || (codec != codecDagPB && codec != codecRaw)) && (v != 0 || codec != codecDagPB) {
		return IPFSCID{}, errors.New("bitswap: unsupported CID prefix")
	}
	return IPFSCID{Version: v, Codec: codec, Hash: h, Digest: sha256Digest(data)}, nil
}
func sha256Digest(b []byte) [32]byte { return sha256.Sum256(b) }
func pbVarint(b []byte, field int, v uint64) []byte {
	b = appendUvarint(b, uint64(field<<3))
	return appendUvarint(b, v)
}
func pbBytes(b []byte, field int, v []byte) []byte {
	b = appendUvarint(b, uint64(field<<3|2))
	b = appendUvarint(b, uint64(len(v)))
	return append(b, v...)
}
func appendUvarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}
func pbField(b []byte) (int, int, []byte, []byte, error) {
	key, n := binary.Uvarint(b)
	if n <= 0 || key>>3 == 0 {
		return 0, 0, nil, nil, errors.New("bitswap: malformed protobuf key")
	}
	f, w := int(key>>3), int(key&7)
	b = b[n:]
	switch w {
	case 0:
		_, n = binary.Uvarint(b)
		if n <= 0 {
			return 0, 0, nil, nil, errors.New("bitswap: malformed varint")
		}
		return f, w, b[:n], b[n:], nil
	case 2:
		z, n := binary.Uvarint(b)
		if n <= 0 || z > maxBitswapMessage || z > uint64(len(b)-n) {
			return 0, 0, nil, nil, errors.New("bitswap: malformed bytes field")
		}
		return f, w, b[n : n+int(z)], b[n+int(z):], nil
	default:
		return 0, 0, nil, nil, errors.New("bitswap: unsupported protobuf wire type")
	}
}
func writeDelimited(w io.Writer, b []byte) error {
	h := appendUvarint(nil, uint64(len(b)))
	if _, e := w.Write(h); e != nil {
		return e
	}
	_, e := w.Write(b)
	return e
}
func readDelimited(r *bufio.Reader, max int) ([]byte, error) {
	n, e := binary.ReadUvarint(r)
	if e != nil {
		return nil, e
	}
	if n == 0 || n > uint64(max) {
		return nil, errors.New("bitswap: message size limit")
	}
	b := make([]byte, int(n))
	_, e = io.ReadFull(r, b)
	return b, e
}
