package cluster

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Invite token limits (docs/cluster.md "Invites and join").
const (
	TokenPrefix   = "nuc1"
	MaxTokenBytes = 4 << 10
	MaxBoot       = 8
	MaxBootAddrs  = 8
)

var (
	ErrToken   = errors.New("cluster: invalid invite token")
	ErrExpired = errors.New("cluster: invite expired")
	b32        = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// Boot is a bootstrap member named by a token.
type Boot struct {
	Peer  wire.PeerID
	Addrs []string
}

// Token is a decoded invite token. Exactly one of Peer (pre-authorized) and Secret
// (secret invite, with InviteID) is set.
type Token struct {
	Genesis  Genesis
	Keys     []string // admin did:keys (public)
	Boot     []Boot
	Expires  time.Time
	Peer     wire.PeerID
	InviteID string
	Secret   []byte
}

type bootWire struct {
	Peer  []byte   `drisl:"peer"`
	Addrs []string `drisl:"addrs"`
}

type inviteWire struct {
	ID     string `drisl:"id"`
	Secret []byte `drisl:"secret"`
}

type tokenWire struct {
	V       int64       `drisl:"v"`
	Genesis Genesis     `drisl:"genesis"`
	Keys    []string    `drisl:"keys"`
	Boot    []bootWire  `drisl:"boot"`
	Expires string      `drisl:"expires"`
	Peer    []byte      `drisl:"peer,omitempty"`
	Invite  *inviteWire `drisl:"invite,omitempty"`
}

// ClusterID is the token's cluster ID.
func (t *Token) ClusterID() (cid.CID, error) { return t.Genesis.ID() }

// FormatTime is the datetime form used in records and tokens.
func FormatTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// Encode returns the nuc1… form.
func (t *Token) Encode() (string, error) {
	if err := t.check(); err != nil {
		return "", err
	}
	w := tokenWire{V: 1, Genesis: t.Genesis, Keys: t.Keys, Boot: []bootWire{}, Expires: FormatTime(t.Expires)}
	if w.Keys == nil {
		w.Keys = []string{}
	}
	for _, b := range t.Boot {
		addrs := b.Addrs
		if addrs == nil {
			addrs = []string{}
		}
		w.Boot = append(w.Boot, bootWire{Peer: b.Peer.Bytes(), Addrs: addrs})
	}
	if t.Peer != "" {
		w.Peer = t.Peer.Bytes()
	} else {
		w.Invite = &inviteWire{ID: t.InviteID, Secret: t.Secret}
	}
	body, err := drisl.Marshal(w)
	if err != nil {
		return "", err
	}
	if len(body) > MaxTokenBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds %d (fewer boot addresses?)", ErrToken, len(body), MaxTokenBytes)
	}
	sum := sha256.Sum256(body)
	return TokenPrefix + strings.ToLower(b32.EncodeToString(append(body, sum[:4]...))), nil
}

func (t *Token) check() error {
	if err := t.Genesis.Validate(); err != nil {
		return err
	}
	if t.Genesis.Mode == ModeLocal {
		return fmt.Errorf("%w: the local cluster cannot be joined", ErrToken)
	}
	if len(t.Boot) == 0 || len(t.Boot) > MaxBoot {
		return fmt.Errorf("%w: needs 1 to %d bootstrap members", ErrToken, MaxBoot)
	}
	for _, b := range t.Boot {
		if _, err := wire.PeerIDFromBytes(b.Peer.Bytes()); err != nil {
			return fmt.Errorf("%w: boot peer: %v", ErrToken, err)
		}
		if len(b.Addrs) > MaxBootAddrs {
			return fmt.Errorf("%w: more than %d addresses for a boot peer", ErrToken, MaxBootAddrs)
		}
		for _, a := range b.Addrs {
			if _, err := wire.ParseMultiaddr(a, 1024); err != nil {
				return fmt.Errorf("%w: boot address %q: %v", ErrToken, a, err)
			}
		}
	}
	for _, k := range t.Keys {
		if _, err := parseP256(k); err != nil {
			return fmt.Errorf("%w: key %q: %v", ErrToken, k, err)
		}
	}
	switch {
	case t.Peer != "" && t.Secret == nil:
		if _, err := wire.PeerIDFromBytes(t.Peer.Bytes()); err != nil {
			return fmt.Errorf("%w: peer: %v", ErrToken, err)
		}
	case t.Peer == "" && len(t.Secret) == 32 && inviteRE.MatchString(t.InviteID):
	default:
		return fmt.Errorf("%w: needs either a PeerId or an invite id and 32-byte secret", ErrToken)
	}
	return nil
}

// ParseToken decodes and validates a nuc1… token. It does not check expiry.
func ParseToken(s string) (*Token, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, TokenPrefix) {
		return nil, fmt.Errorf("%w: does not start with %s", ErrToken, TokenPrefix)
	}
	s = s[len(TokenPrefix):]
	if len(s) > (MaxTokenBytes+4)*8/5+1 {
		return nil, fmt.Errorf("%w: too long", ErrToken)
	}
	raw, err := b32.DecodeString(strings.ToUpper(s))
	if err != nil || len(raw) < 5 {
		return nil, fmt.Errorf("%w: not base32", ErrToken)
	}
	body, check := raw[:len(raw)-4], raw[len(raw)-4:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:4], check) {
		return nil, fmt.Errorf("%w: checksum mismatch (was it copied completely?)", ErrToken)
	}
	var w tokenWire
	if err := (drisl.DecOptions{MaxSize: MaxTokenBytes, MaxDepth: 6}).Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToken, err)
	}
	if w.V != 1 {
		return nil, fmt.Errorf("%w: version %d", ErrToken, w.V)
	}
	t := &Token{Genesis: w.Genesis, Keys: w.Keys}
	if t.Expires, err = time.Parse(time.RFC3339, w.Expires); err != nil {
		return nil, fmt.Errorf("%w: expires: %v", ErrToken, err)
	}
	for _, b := range w.Boot {
		id, err := wire.PeerIDFromBytes(b.Peer)
		if err != nil {
			return nil, fmt.Errorf("%w: boot peer: %v", ErrToken, err)
		}
		t.Boot = append(t.Boot, Boot{Peer: id, Addrs: b.Addrs})
	}
	if w.Peer != nil {
		if t.Peer, err = wire.PeerIDFromBytes(w.Peer); err != nil {
			return nil, fmt.Errorf("%w: peer: %v", ErrToken, err)
		}
	}
	if w.Invite != nil {
		t.InviteID, t.Secret = w.Invite.ID, w.Invite.Secret
		if t.Secret == nil {
			t.Secret = []byte{}
		}
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	return t, nil
}

// NewSecret returns a random invite ID and 32-byte secret.
func NewSecret() (id string, secret []byte) {
	secret = make([]byte, 32)
	idb := make([]byte, 10)
	_, _ = rand.Read(secret)
	_, _ = rand.Read(idb)
	return strings.ToLower(b32.EncodeToString(idb)), secret
}

func proofPrivate(secret []byte) ed25519.PrivateKey {
	seed := sha256.Sum256(append([]byte("nu.cluster.invite.v1\x00"), secret...))
	return ed25519.NewKeyFromSeed(seed[:])
}

// ProofKey returns the invite record's proofKey for secret.
func ProofKey(secret []byte) []byte {
	return []byte(proofPrivate(secret).Public().(ed25519.PublicKey))
}

func proofMessage(cluster cid.CID, peer wire.PeerID) []byte {
	m := append([]byte("nu.cluster.join.v1\x00"), cluster.Bytes()...)
	return append(m, peer.Bytes()...)
}

// JoinProof proves knowledge of secret for peer joining cluster.
func JoinProof(secret []byte, cluster cid.CID, peer wire.PeerID) []byte {
	return ed25519.Sign(proofPrivate(secret), proofMessage(cluster, peer))
}

// VerifyJoinProof checks a join proof against an invite's proofKey.
func VerifyJoinProof(proofKey []byte, cluster cid.CID, peer wire.PeerID, proof []byte) bool {
	return len(proofKey) == ed25519.PublicKeySize && ed25519.Verify(proofKey, proofMessage(cluster, peer), proof)
}
