package cluster

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const vectorFile = "../testdata/vectors/nu-cluster/vectors.json"

type vectors struct {
	HLC struct {
		Micros uint64 `json:"micros"`
		TID    string `json:"tid"`
	} `json:"hlc"`
	Genesis struct {
		Record Genesis `json:"record"`
		DRISL  string  `json:"drisl"`
		ID     string  `json:"id"`
	} `json:"genesis"`
	Proof struct {
		Secret   string `json:"secret"`
		ProofKey string `json:"proofKey"`
		Peer     string `json:"peer"`
		Proof    string `json:"proof"`
	} `json:"proof"`
	Tokens struct {
		Preauthorized string `json:"preauthorized"`
		Secret        string `json:"secret"`
	} `json:"tokens"`
	// Policies are dataset records (hex DRISL): replicas k=2, and `replicas: all`,
	// encoded {replicas: 0, all: true} (docs/cluster.md record table).
	Policies struct {
		K2  string `json:"k2"`
		All string `json:"all"`
	} `json:"policies"`
}

func fixedPeer(t *testing.T, i byte) wire.PeerID {
	seed := bytes.Repeat([]byte{i}, ed25519.SeedSize)
	k, err := wire.ParseNodeKey(wire.AppendBytes(wire.AppendUint(nil, 1, uint64(wire.Ed25519)), 2, ed25519.NewKeyFromSeed(seed)), wire.MaxKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().PeerID()
}

func buildVectors(t *testing.T) vectors {
	var v vectors
	v.HLC.Micros = 1767225600000000 + 7
	v.HLC.TID = HLC(v.HLC.Micros).String()

	priv, err := atcrypto.ParsePrivateBytesP256(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := priv.PublicKey()
	g := Genesis{Name: "lab", Mode: ModePrivate, RootKey: pub.DIDKey(), CreatedAt: "2026-01-01T00:00:00Z"}
	id, b, err := drisl.Sum(g)
	if err != nil {
		t.Fatal(err)
	}
	v.Genesis.Record, v.Genesis.DRISL, v.Genesis.ID = g, hex.EncodeToString(b), id.String()

	secret := bytes.Repeat([]byte{0x22}, 32)
	joiner := fixedPeer(t, 3)
	v.Proof.Secret = hex.EncodeToString(secret)
	v.Proof.ProofKey = hex.EncodeToString(ProofKey(secret))
	v.Proof.Peer = joiner.String()
	v.Proof.Proof = hex.EncodeToString(JoinProof(secret, id, joiner))

	boot := []Boot{{Peer: fixedPeer(t, 1), Addrs: []string{"/ip4/10.0.0.1/udp/4171/quic-v1"}}}
	exp := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	pre := &Token{Genesis: g, Keys: []string{pub.DIDKey()}, Boot: boot, Expires: exp, Peer: joiner}
	if v.Tokens.Preauthorized, err = pre.Encode(); err != nil {
		t.Fatal(err)
	}
	sec := &Token{Genesis: g, Boot: boot, Expires: exp, InviteID: "abcdefghijklmnop", Secret: secret}
	if v.Tokens.Secret, err = sec.Encode(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		out *string
		pol Policy
	}{{&v.Policies.K2, Policy{Replicas: 2, Spread: "zone"}}, {&v.Policies.All, Policy{All: true}}} {
		b, err := encodeRecord(&Dataset{Source: Source{Local: true}, Policy: p.pol, HLC: v.HLC.TID})
		if err != nil {
			t.Fatal(err)
		}
		*p.out = hex.EncodeToString(b)
	}
	return v
}

// The HLC, genesis/cluster ID, join proof and invite token bytes are permanent
// contracts (docs/cluster.md "State encoding and evaluation", "Invites and join").
func TestVectors(t *testing.T) {
	got := buildVectors(t)
	if os.Getenv("NU_UPDATE_VECTORS") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll("../testdata/vectors/nu-cluster", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var want vectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("vectors changed:\n got %+v\nwant %+v", got, want)
	}
	if h, err := ParseHLC(want.HLC.TID); err != nil || uint64(h) != want.HLC.Micros {
		t.Fatalf("ParseHLC = %d, %v", h, err)
	}
	for _, s := range []string{want.Tokens.Preauthorized, want.Tokens.Secret} {
		tok, err := ParseToken(s)
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := tok.ClusterID(); id.String() != want.Genesis.ID {
			t.Fatalf("token cluster %s", id)
		}
		if again, _ := tok.Encode(); again != s {
			t.Fatal("token does not re-encode")
		}
	}
	for _, c := range []struct {
		hex  string
		want Policy
	}{{want.Policies.K2, Policy{Replicas: 2, Spread: "zone"}}, {want.Policies.All, Policy{All: true}}} {
		b, _ := hex.DecodeString(c.hex)
		r, err := DecodeRecord(NSDataset+"/photos", b)
		if err != nil {
			t.Fatal(err)
		}
		if p := r.Value.(*Dataset).Policy; p.Replicas != c.want.Replicas || p.All != c.want.All || p.Spread != c.want.Spread {
			t.Fatalf("policy decoded as %+v, want %+v", p, c.want)
		}
	}
	tok, _ := ParseToken(want.Tokens.Secret)
	pk, _ := hex.DecodeString(want.Proof.ProofKey)
	proof, _ := hex.DecodeString(want.Proof.Proof)
	id, _ := tok.ClusterID()
	if !VerifyJoinProof(pk, id, fixedPeer(t, 3), proof) || VerifyJoinProof(pk, id, fixedPeer(t, 4), proof) {
		t.Fatal("join proof is not bound to the joiner's PeerId")
	}
	bad := []byte(want.Tokens.Secret)
	bad[20] ^= 1
	if _, err := ParseToken(string(bad)); err == nil {
		t.Fatal("corrupted token accepted")
	}
}

func FuzzParseToken(f *testing.F) {
	raw, _ := os.ReadFile(vectorFile)
	var v vectors
	_ = json.Unmarshal(raw, &v)
	f.Add(v.Tokens.Preauthorized)
	f.Add(v.Tokens.Secret)
	f.Add("nuc1")
	f.Add("nuc1aaaa")
	f.Fuzz(func(t *testing.T, s string) {
		tok, err := ParseToken(s)
		if err != nil {
			return
		}
		if _, err := tok.Encode(); err != nil {
			t.Fatalf("parsed token does not encode: %v", err)
		}
	})
}

func FuzzDecodeRecord(f *testing.F) {
	h := HLC(1767225600000000).String()
	for _, rec := range []any{
		&Member{Roles: []string{RoleStorage}, State: StateActive, Addrs: []string{"/ip4/10.0.0.1/udp/4171/quic-v1"}, HLC: h},
		&Dataset{Source: Source{Local: true}, Policy: Policy{Replicas: 2}, HLC: h},
		&Head{Target: "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", Kind: "file", HLC: h},
		&Key{Role: RoleAdmin, HLC: h},
		&Invite{ProofKey: make([]byte, 32), Roles: []string{RoleClient}, Expires: "2026-01-01T00:00:00Z", HLC: h},
	} {
		b, _ := drisl.Marshal(rec)
		f.Add(b)
	}
	tb, _ := tombstone(NSDataset, 1)
	f.Add(tb)
	paths := []string{NSMember + "/12D3KooWGzBkk2t7Jb6bYQDwT6iA8GxwYyVdJXQ4bUQVCWzK1gLt", NSDataset + "/photos", NSHead + "/photos",
		NSKey + "/did:key:zDnaeTiq1PdzvZXUaMdezchcMJQpBdH2VN4pgrrEhMCCbmwSb", NSInvite + "/abcdefghijklmnop"}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, p := range paths {
			r, err := DecodeRecord(p, b)
			if err == nil && r.Value == nil && !r.Deleted && r.Collection != NSHistory && r.Collection != NSDrop {
				t.Fatalf("%s decoded without a value", p)
			}
		}
	})
}
