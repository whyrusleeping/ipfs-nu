package wire_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const maxBytes = 8192

var recordLimits = wire.RecordLimits{MaxBytes: maxBytes, MaxAddresses: 16, MaxAddressBytes: 1024}

type keyVectors struct {
	Ed25519Private, Ed25519Public string
	Verification                  []struct{ Type, Public, Signature string }
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func vectors(t testing.TB) keyVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors/keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var v keyVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func fixtureKey(t testing.TB) *wire.NodeKey {
	t.Helper()
	k, err := wire.ParseNodeKey(unhex(t, vectors(t).Ed25519Private), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func fixtureAddress(t testing.TB) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr("/ip4/127.0.0.1/udp/1234/quic-v1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestKeyVectors(t *testing.T) {
	v := vectors(t)
	k := fixtureKey(t)
	if !bytes.Equal(k.Public().Marshal(), unhex(t, v.Ed25519Public)) || !bytes.Equal(k.Marshal(), unhex(t, v.Ed25519Private)) {
		t.Fatal("spec Ed25519 key pair")
	}
	message := []byte("Libp2p is the _best_!")
	for _, v := range v.Verification {
		t.Run(v.Type, func(t *testing.T) {
			raw := unhex(t, v.Public)
			pub, err := wire.ParsePublicKey(raw, maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pub.Marshal(), raw) {
				t.Fatal("key round trip")
			}
			sig := unhex(t, v.Signature)
			if err := pub.Verify(message, sig); err != nil {
				t.Fatal(err)
			}
			sig[0] ^= 1
			if pub.Verify(message, sig) == nil {
				t.Fatal("corrupt signature accepted")
			}
			if _, err := wire.PeerIDFromBytes(pub.PeerID().Bytes()); err != nil {
				t.Fatal(err)
			}
			if pub.Type() == wire.Secp256k1 && pub.PeerID().Bytes()[0] != 0 {
				t.Fatal("secp key must use identity")
			}
			if pub.Type() != wire.Secp256k1 && pub.PeerID().Bytes()[0] != 0x12 {
				t.Fatal("large key must use SHA-256")
			}
		})
	}
	if err := k.Public().Verify(message, k.Sign(message)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{{8, 1, 18, 0}, append(unhex(t, v.Ed25519Public), 0x18, 1), append([]byte{0x88, 0}, unhex(t, v.Ed25519Public)[1:]...)} {
		if _, err := wire.ParsePublicKey(bad, maxBytes); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
func TestNodeKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.key")
	k, err := wire.LoadOrCreateNodeKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key permissions", info, err)
	}
	loaded, err := wire.LoadOrCreateNodeKey(path)
	if err != nil || loaded.Public().PeerID() != k.Public().PeerID() {
		t.Fatal("identity changed", err)
	}
	if err := k.Store(path); !os.IsExist(err) {
		t.Fatal("overwrote node key", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.LoadNodeKey(path); err == nil {
		t.Fatal("insecure file accepted")
	}
}
func TestPeerIDVectors(t *testing.T) {
	// Text examples from libp2p/specs/peer-ids/peer-ids.md.
	for _, text := range []string{
		"bafzbeie5745rpv2m6tjyuugywy4d5ewrqgqqhfnf445he3omzpjbx5xqxe",
		"QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N",
		"12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA",
	} {
		id, err := wire.ParsePeerID(text)
		if err != nil {
			t.Fatal(text, err)
		}
		for _, text := range []string{id.String(), id.Base32(), id.Base36()} {
			again, err := wire.ParsePeerID(text)
			if err != nil || again != id {
				t.Fatal("peer id text round trip", text, err)
			}
		}
	}
	// A CID with a different codec is not a PeerId.
	if _, err := wire.ParsePeerID("bafybeie5745rpv2m6tjyuugywy4d5ewrqgqqhfnf445he3omzpjbx5xqxe"); err == nil {
		t.Fatal("accepted non-libp2p-key CID")
	}
}
func TestMultiaddrVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors/multiaddr.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Text, Hex string }
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		a, err := wire.ParseMultiaddr(v.Text, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a.Bytes(), unhex(t, v.Hex)) {
			t.Fatal("multiaddr vector", v.Text)
		}
		a, err = wire.MultiaddrFromBytes(unhex(t, v.Hex), 1024)
		if err != nil || a.String() != v.Text {
			t.Fatal("binary round trip", v.Text, err)
		}
	}
	for _, s := range []string{
		"/ip6/::1/udp/4171/quic-v1",
		"/dns/example.org/udp/4171/quic-v1",
		"/dns4/example.org/udp/4171/quic-v1",
		"/dns6/example.org/udp/4171/quic-v1",
		"/dnsaddr/example.org/p2p/12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA/p2p-circuit",
	} {
		a, err := wire.ParseMultiaddr(s, 1024)
		if err != nil {
			t.Fatal(err)
		}
		again, err := wire.MultiaddrFromBytes(a.Bytes(), 1024)
		if err != nil || again.String() != s {
			t.Fatal(s, err)
		}
	}
	// Unregistered code: the rest cannot be framed without knowing the protocol.
	raw := []byte{4, 127, 0, 0, 1, 0xff, 0xff, 0x7f, 0, 1, 255}
	a, err := wire.MultiaddrFromBytes(raw, 1024)
	if err != nil || !a.Opaque() || !bytes.Equal(raw, a.Bytes()) {
		t.Fatal("opaque address lost", err)
	}
	if _, err := a.Text(); err == nil {
		t.Fatal("invented text for unregistered code")
	}
	if _, err := wire.MultiaddrFromBytes([]byte{0x84, 0, 127, 0, 0, 1}, 1024); err == nil {
		t.Fatal("nonminimal protocol code")
	}
}
func TestSignedPeerRecord(t *testing.T) {
	k := fixtureKey(t)
	data, err := os.ReadFile("testdata/vectors/peer-record.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ PeerID, Base32, Record, Envelope string }
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	vector := wire.PeerRecord{PeerID: k.Public().PeerID(), Seq: 1, Addresses: []wire.Multiaddr{fixtureAddress(t)}}
	payload, err := vector.Marshal(recordLimits)
	if err != nil || !bytes.Equal(payload, unhex(t, golden.Record)) {
		t.Fatal("go-libp2p record vector", err)
	}
	env, err := vector.Seal(k, recordLimits)
	if err != nil || !bytes.Equal(env, unhex(t, golden.Envelope)) {
		t.Fatal("go-libp2p envelope vector", err)
	}
	if _, err := wire.OpenPeerRecord(unhex(t, golden.Envelope), recordLimits); err != nil {
		t.Fatal(err)
	}
	if k.Public().PeerID().String() != golden.PeerID || k.Public().PeerID().Base32() != golden.Base32 {
		t.Fatal("go-libp2p PeerId vector")
	}
	r := wire.PeerRecord{PeerID: k.Public().PeerID(), Seq: ^uint64(0), Addresses: []wire.Multiaddr{fixtureAddress(t)}}
	sealed, err := r.Seal(k, recordLimits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.OpenPeerRecord(sealed, recordLimits)
	if err != nil || decoded.PeerID != r.PeerID || decoded.Seq != r.Seq || len(decoded.Addresses) != 1 || !bytes.Equal(decoded.Addresses[0].Bytes(), r.Addresses[0].Bytes()) {
		t.Fatal("peer record round trip", err)
	}
	if _, err := wire.OpenEnvelope(sealed, "wrong-domain", maxBytes); err == nil {
		t.Fatal("domain not bound")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := wire.OpenPeerRecord(sealed, recordLimits); err == nil {
		t.Fatal("signature not checked")
	}
	sealed, err = r.Seal(k, recordLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.OpenPeerRecord(sealed, wire.RecordLimits{MaxBytes: maxBytes, MaxAddresses: 0, MaxAddressBytes: 1024}); err == nil {
		t.Fatal("address count limit")
	}
	if _, err := wire.OpenPeerRecord(sealed, wire.RecordLimits{MaxBytes: len(sealed) - 1, MaxAddresses: 16, MaxAddressBytes: 1024}); err == nil {
		t.Fatal("message size limit")
	}
	other, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	r.PeerID = other.Public().PeerID()
	payload, err = r.Marshal(recordLimits)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := wire.SealEnvelope(k, wire.PeerRecordDomain, []byte{3, 1}, payload, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.OpenPeerRecord(forged, recordLimits); err == nil {
		t.Fatal("record identity not bound to signer")
	}
	// RFC 0002 permits the payload-type hint to be empty.
	empty, err := wire.SealEnvelope(k, "test-domain", nil, nil, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.OpenEnvelope(empty, "test-domain", maxBytes); err != nil {
		t.Fatal(err)
	}
}
func FuzzKeyProtobuf(f *testing.F) {
	v := vectors(f)
	f.Add(unhex(f, v.Ed25519Public))
	f.Add(unhex(f, v.Ed25519Private))
	f.Add([]byte{8, 1, 18, 0})
	for _, v := range v.Verification {
		f.Add(unhex(f, v.Public))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if k, err := wire.ParsePublicKey(b, maxBytes); err == nil && !bytes.Equal(k.Marshal(), b) {
			t.Fatal("key encoding")
		}
		if k, err := wire.ParseNodeKey(b, maxBytes); err == nil {
			if _, err := wire.ParseNodeKey(k.Marshal(), maxBytes); err != nil {
				t.Fatal(err)
			}
		}
	})
}
func FuzzMultiaddrBytes(f *testing.F) {
	f.Add(fixtureAddress(f).Bytes())
	f.Add([]byte{0xff, 0xff, 0x7f, 0, 1, 255})
	f.Add([]byte{4})
	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := wire.MultiaddrFromBytes(b, 1024)
		if err != nil {
			return
		}
		if !bytes.Equal(a.Bytes(), b) {
			t.Fatal("lost opaque bytes")
		}
		if !a.Opaque() && len(b) > 0 {
			s, err := a.Text()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wire.ParseMultiaddr(s, 8192); err != nil {
				t.Fatal(err)
			}
		}
	})
}
func FuzzSignedEnvelope(f *testing.F) {
	k := fixtureKey(f)
	r := wire.PeerRecord{PeerID: k.Public().PeerID(), Seq: 1, Addresses: []wire.Multiaddr{fixtureAddress(f)}}
	b, err := r.Seal(k, recordLimits)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add([]byte{10, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = wire.OpenEnvelope(b, wire.PeerRecordDomain, maxBytes)
		_, _ = wire.OpenPeerRecord(b, recordLimits)
	})
}
func FuzzPeerID(f *testing.F) {
	f.Add(fixtureKey(f).Public().PeerID().Base36())
	f.Add("QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := wire.ParsePeerID(s)
		if err != nil {
			return
		}
		for _, enc := range []string{id.String(), id.Base32(), id.Base36()} {
			got, err := wire.ParsePeerID(enc)
			if err != nil || got != id {
				t.Fatal("id round trip", err)
			}
		}
	})
}
func FuzzProtobuf(f *testing.F) {
	f.Add(wire.AppendUint(wire.AppendBytes(nil, 1, []byte{1}), 2, ^uint64(0)))
	f.Add([]byte{0x0a, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := wire.NewProtoReader(b, maxBytes)
		if err != nil {
			return
		}
		for {
			_, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
		}
		_, _ = wire.ParsePeerRecord(b, recordLimits)
	})
}
