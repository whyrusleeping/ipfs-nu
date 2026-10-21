package wire

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"io"
	"os"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

type KeyType uint64

const (
	RSA KeyType = iota
	Ed25519
	Secp256k1
	ECDSA
	MaxKeyBytes = 8192
)

// PublicKey is an immutable, validated libp2p public key. Only verification is
// exposed for non-Ed25519 identities.
type PublicKey struct {
	kind KeyType
	raw  []byte
	key  crypto.PublicKey
}

func (k PublicKey) Type() KeyType   { return k.kind }
func (k PublicKey) Marshal() []byte { return keyMessage(k.kind, k.raw) }
func keyMessage(kind KeyType, data []byte) []byte {
	return AppendBytes(AppendUint(nil, 1, uint64(kind)), 2, data)
}

// ParsePublicKey requires the deterministic protobuf encoding specified by
// libp2p peer-ids (exactly Type then Data, minimally encoded).
func ParsePublicKey(b []byte, maxBytes int) (PublicKey, error) {
	var k PublicKey
	kind, raw, err := parseKeyMessage(b, maxBytes)
	if err != nil {
		return k, err
	}
	k.kind = kind
	k.raw = bytes.Clone(raw)
	switch kind {
	case Ed25519:
		if len(raw) != ed25519.PublicKeySize {
			return PublicKey{}, ErrFormat
		}
		k.key = ed25519.PublicKey(k.raw)
	case Secp256k1:
		if len(raw) != 33 {
			return PublicKey{}, ErrFormat
		}
		k.key, err = secp.ParsePubKey(raw)
	case ECDSA, RSA:
		k.key, err = x509.ParsePKIXPublicKey(raw)
		if err != nil {
			return PublicKey{}, err
		}
		switch p := k.key.(type) {
		case *rsa.PublicKey:
			if kind != RSA || p.N.BitLen() < 2048 || p.N.BitLen() > 8192 {
				return PublicKey{}, ErrFormat
			}
		case *ecdsa.PublicKey:
			if kind != ECDSA {
				return PublicKey{}, ErrFormat
			}
		default:
			return PublicKey{}, ErrFormat
		}
	default:
		return PublicKey{}, ErrFormat
	}
	if err != nil {
		return PublicKey{}, err
	}
	return k, nil
}

func parseKeyMessage(b []byte, maxBytes int) (KeyType, []byte, error) {
	if maxBytes > MaxKeyBytes {
		maxBytes = MaxKeyBytes
	}
	r, err := NewProtoReader(b, maxBytes)
	if err != nil {
		return 0, nil, err
	}
	typ, err := r.Next()
	if err != nil {
		return 0, nil, err
	}
	data, err := r.Next()
	if err != nil {
		return 0, nil, err
	}
	if typ.Number != 1 || typ.Type != 0 || data.Number != 2 || data.Type != 2 {
		return 0, nil, ErrFormat
	}
	if _, err = r.Next(); err != io.EOF {
		return 0, nil, ErrFormat
	}
	kind := KeyType(typ.Uint)
	if !bytes.Equal(b, keyMessage(kind, data.Bytes)) {
		return 0, nil, ErrFormat
	}
	return kind, data.Bytes, nil
}

func (k PublicKey) Verify(message, signature []byte) error {
	digest := sha256.Sum256(message)
	valid := false
	switch p := k.key.(type) {
	case ed25519.PublicKey:
		valid = ed25519.Verify(p, message, signature)
	case *rsa.PublicKey:
		valid = rsa.VerifyPKCS1v15(p, crypto.SHA256, digest[:], signature) == nil
	case *ecdsa.PublicKey:
		valid = ecdsa.VerifyASN1(p, digest[:], signature)
	case *secp.PublicKey:
		sig, err := secpecdsa.ParseDERSignature(signature)
		valid = err == nil && sig.Verify(digest[:], p)
	}
	if !valid {
		return ErrSignature
	}
	return nil
}

// NodeKey is an immutable Ed25519 node identity.
type NodeKey struct{ key ed25519.PrivateKey }

func GenerateNodeKey() (*NodeKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &NodeKey{priv}, nil
}
func (k *NodeKey) Public() PublicKey {
	pub := bytes.Clone(k.key[32:])
	return PublicKey{kind: Ed25519, raw: pub, key: ed25519.PublicKey(pub)}
}
func (k *NodeKey) Sign(b []byte) []byte { return ed25519.Sign(k.key, b) }
func (k *NodeKey) Marshal() []byte      { return keyMessage(Ed25519, k.key) }

// ParseNodeKey imports the standard libp2p PrivateKey protobuf. The historical
// 96-byte Ed25519 representation is accepted only when both public copies agree.
func ParseNodeKey(b []byte, maxBytes int) (*NodeKey, error) {
	kind, raw, err := parseKeyMessage(b, maxBytes)
	if err != nil {
		return nil, err
	}
	if kind != Ed25519 {
		return nil, ErrFormat
	}
	if len(raw) == 96 {
		if !bytes.Equal(raw[32:64], raw[64:]) {
			return nil, ErrFormat
		}
		raw = raw[:64]
	}
	if len(raw) != 64 {
		return nil, ErrFormat
	}
	priv := ed25519.NewKeyFromSeed(raw[:32])
	if !bytes.Equal(priv, raw) {
		return nil, ErrFormat
	}
	return &NodeKey{priv}, nil
}

// Store creates a new 0600 file and refuses to overwrite an existing identity.
// Failed writes are removed. Callers should put the file in a private directory.
func (k *NodeKey) Store(path string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	_, err = f.Write(k.Marshal())
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func LoadNodeKey(path string) (*NodeKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("wire: node key must be a regular 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxKeyBytes+1))
	if err != nil {
		return nil, err
	}
	return ParseNodeKey(b, MaxKeyBytes)
}
func LoadOrCreateNodeKey(path string) (*NodeKey, error) {
	k, err := LoadNodeKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	k, err = GenerateNodeKey()
	if err != nil {
		return nil, err
	}
	if err = k.Store(path); errors.Is(err, os.ErrExist) {
		return LoadNodeKey(path)
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}
