// Package noise implements the libp2p Noise XX transport. Callers negotiate
// /noise before Handshake and a stream multiplexer afterwards. No early-data
// extensions are advertised. This implementation requires independent security
// review before production deployment.
package noise

import (
	"context"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/crypto/chacha20poly1305"
)

const ID = "/noise"
const MaxRecord = 65535
const MaxHandshake = 16 << 10
const signaturePrefix = "noise-libp2p-static-key:"

var ErrIdentity = errors.New("noise: unexpected peer identity")
var ErrProtocol = errors.New("noise: invalid message")

type cipherState struct {
	aead  cipher.AEAD
	nonce uint64
}

func newCipher(key []byte) cipherState {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err)
	}
	return cipherState{aead: a}
}
func (c *cipherState) crypt(b, ad []byte, encrypt bool) ([]byte, error) {
	if c.nonce == math.MaxUint64 {
		return nil, errors.New("noise: nonce exhausted")
	}
	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[4:], c.nonce)
	c.nonce++
	if encrypt {
		return c.aead.Seal(nil, nonce[:], b, ad), nil
	}
	return c.aead.Open(nil, nonce[:], b, ad)
}

type symmetric struct {
	h, ck [32]byte
	cipherState
}

func newSymmetric() *symmetric {
	// The protocol name is <= HASHLEN, so Noise pads rather than hashes it.
	var h [32]byte
	copy(h[:], "Noise_XX_25519_ChaChaPoly_SHA256")
	s := &symmetric{h: h, ck: h}
	s.mixHash(nil)
	return s
}
func (s *symmetric) mixHash(b []byte) {
	h := sha256.New()
	h.Write(s.h[:])
	h.Write(b)
	copy(s.h[:], h.Sum(nil))
}
func mac(key, b []byte) []byte { h := hmac.New(sha256.New, key); h.Write(b); return h.Sum(nil) }
func derive(ck, input []byte) ([]byte, []byte) {
	t := mac(ck, input)
	a := mac(t, []byte{1})
	b := mac(t, append(append([]byte{}, a...), 2))
	return a, b
}
func (s *symmetric) dh(local *ecdh.PrivateKey, remote []byte) error {
	pub, err := ecdh.X25519().NewPublicKey(remote)
	if err != nil {
		return err
	}
	shared, err := local.ECDH(pub)
	if err != nil {
		return err
	}
	ck, key := derive(s.ck[:], shared)
	copy(s.ck[:], ck)
	s.cipherState = newCipher(key)
	return nil
}
func (s *symmetric) encrypt(b []byte) ([]byte, error) {
	out, err := s.crypt(b, s.h[:], true)
	if err == nil {
		s.mixHash(out)
	}
	return out, err
}
func (s *symmetric) decrypt(b []byte) ([]byte, error) {
	out, err := s.crypt(b, s.h[:], false)
	if err == nil {
		s.mixHash(b)
	}
	return out, err
}
func payload(key *wire.NodeKey, static []byte) []byte {
	return wire.AppendBytes(wire.AppendBytes(nil, 1, key.Public().Marshal()), 2, key.Sign(append([]byte(signaturePrefix), static...)))
}
func verifyPayload(b, static []byte, expected wire.PeerID) (wire.PublicKey, error) {
	var zero wire.PublicKey
	r, err := wire.NewProtoReader(b, MaxHandshake)
	if err != nil {
		return zero, err
	}
	var pub, sig []byte
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return zero, err
		}
		switch f.Number {
		case 1:
			if pub != nil || f.Type != 2 {
				return zero, ErrProtocol
			}
			pub = f.Bytes
		case 2:
			if sig != nil || f.Type != 2 {
				return zero, ErrProtocol
			}
			sig = f.Bytes
			// Unknown extensions are bounded by the enclosing payload and not advertised.
		}
	}
	key, err := wire.ParsePublicKey(pub, wire.MaxKeyBytes)
	if err != nil {
		return zero, err
	}
	if expected != "" && key.PeerID() != expected {
		return zero, ErrIdentity
	}
	if err := key.Verify(append([]byte(signaturePrefix), static...), sig); err != nil {
		return zero, err
	}
	return key, nil
}
func readRecord(r io.Reader, max int) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if n > max {
		return nil, wire.ErrLimit
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func writeRecord(w io.Writer, b []byte) error {
	if len(b) > MaxRecord {
		return wire.ErrLimit
	}
	out := binary.BigEndian.AppendUint16(nil, uint16(len(b)))
	out = append(out, b...)
	for len(out) > 0 {
		n, err := w.Write(out)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(out) {
			return io.ErrShortWrite
		}
		out = out[n:]
	}
	return nil
}

// Conn is an authenticated encrypted byte stream. One reader and one writer can
// operate concurrently. Any record error closes the underlying connection; it
// must never be retried with a reused or desynchronized nonce.
type Conn struct {
	net.Conn
	peer            wire.PublicKey
	readMu, writeMu sync.Mutex
	recv, send      cipherState
	pending         []byte
}

func (c *Conn) PublicKey() wire.PublicKey { return c.peer }
func (c *Conn) PeerID() wire.PeerID       { return c.peer.PeerID() }
func (c *Conn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for empty := 0; len(c.pending) == 0; empty++ {
		if empty >= 16 {
			c.Close()
			return 0, ErrProtocol
		}
		msg, err := readRecord(c.Conn, MaxRecord)
		if err != nil {
			c.Close()
			return 0, err
		}
		c.pending, err = c.recv.crypt(msg, nil, false)
		if err != nil {
			c.Close()
			return 0, fmt.Errorf("noise: record authentication: %w", err)
		}
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
func (c *Conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	total := 0
	for len(b) > 0 {
		n := min(len(b), MaxRecord-chacha20poly1305.Overhead)
		msg, err := c.send.crypt(b[:n], nil, true)
		if err == nil {
			err = writeRecord(c.Conn, msg)
		}
		if err != nil {
			c.Close()
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}

// Handshake performs XX with an empty prologue. It owns raw (also on failure),
// clears its deadline on success, and caps the handshake at ten seconds even if
// ctx has no deadline. Initiators MUST provide the expected remote PeerID.
func Handshake(ctx context.Context, raw net.Conn, key *wire.NodeKey, expected wire.PeerID, initiator bool) (conn *Conn, err error) {
	defer func() {
		if err != nil {
			raw.Close()
			err = fmt.Errorf("noise: handshake: %w", err)
		}
	}()
	if key == nil || (initiator && expected == "") {
		return nil, ErrIdentity
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = raw.SetDeadline(deadline); err != nil {
		return nil, err
	}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Now()); close(canceled) })
	defer func() {
		if !stop() {
			<-canceled
		}
		if ctx.Err() != nil && err == nil {
			conn = nil
			err = ctx.Err()
		}
		if err == nil {
			err = raw.SetDeadline(time.Time{})
		}
	}()
	static, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	s := newSymmetric()
	var peer wire.PublicKey
	if initiator {
		e := ephemeral.PublicKey().Bytes()
		s.mixHash(e)
		s.mixHash(nil)
		if err = writeRecord(raw, e); err != nil {
			return nil, err
		}
		msg, eerr := readRecord(raw, MaxHandshake)
		if eerr != nil {
			return nil, eerr
		}
		if len(msg) < 96 {
			return nil, ErrProtocol
		}
		remoteE := msg[:32]
		s.mixHash(remoteE)
		if err = s.dh(ephemeral, remoteE); err != nil {
			return nil, err
		}
		remoteS, eerr := s.decrypt(msg[32:80])
		if eerr != nil {
			return nil, eerr
		}
		if err = s.dh(ephemeral, remoteS); err != nil {
			return nil, err
		}
		p, eerr := s.decrypt(msg[80:])
		if eerr != nil {
			return nil, eerr
		}
		peer, err = verifyPayload(p, remoteS, expected)
		if err != nil {
			return nil, err
		}
		out, eerr := s.encrypt(static.PublicKey().Bytes())
		if eerr != nil {
			return nil, eerr
		}
		if err = s.dh(static, remoteE); err != nil {
			return nil, err
		}
		p, eerr = s.encrypt(payload(key, static.PublicKey().Bytes()))
		if eerr != nil {
			return nil, eerr
		}
		if err = writeRecord(raw, append(out, p...)); err != nil {
			return nil, err
		}
	} else {
		msg, eerr := readRecord(raw, MaxHandshake)
		if eerr != nil {
			return nil, eerr
		}
		if len(msg) < 32 {
			return nil, ErrProtocol
		}
		remoteE := msg[:32]
		s.mixHash(remoteE)
		s.mixHash(msg[32:])
		out := ephemeral.PublicKey().Bytes()
		s.mixHash(out)
		if err = s.dh(ephemeral, remoteE); err != nil {
			return nil, err
		}
		enc, eerr := s.encrypt(static.PublicKey().Bytes())
		if eerr != nil {
			return nil, eerr
		}
		out = append(out, enc...)
		if err = s.dh(static, remoteE); err != nil {
			return nil, err
		}
		enc, eerr = s.encrypt(payload(key, static.PublicKey().Bytes()))
		if eerr != nil {
			return nil, eerr
		}
		if err = writeRecord(raw, append(out, enc...)); err != nil {
			return nil, err
		}
		msg, eerr = readRecord(raw, MaxHandshake)
		if eerr != nil {
			return nil, eerr
		}
		if len(msg) < 64 {
			return nil, ErrProtocol
		}
		remoteS, eerr := s.decrypt(msg[:48])
		if eerr != nil {
			return nil, eerr
		}
		if err = s.dh(ephemeral, remoteS); err != nil {
			return nil, err
		}
		p, eerr := s.decrypt(msg[48:])
		if eerr != nil {
			return nil, eerr
		}
		peer, err = verifyPayload(p, remoteS, expected)
		if err != nil {
			return nil, err
		}
	}
	first, second := derive(s.ck[:], nil)
	conn = &Conn{Conn: raw, peer: peer, send: newCipher(first), recv: newCipher(second)}
	if !initiator {
		conn.send, conn.recv = conn.recv, conn.send
	}
	return conn, nil
}
