package wire_test

import (
	"crypto/x509"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestTLSVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/vectors/tls.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ PeerID, Certificate string }
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, v := range vectors {
		key, err := wire.VerifyTLSCertificate([][]byte{unhex(t, v.Certificate)}, "", now, 16<<10)
		if v.PeerID == "" {
			if err == nil {
				t.Fatal("invalid TLS spec vector accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(v.PeerID, err)
		}
		if key.PeerID().String() != v.PeerID {
			t.Fatal("TLS spec PeerId", key.PeerID())
		}
	}
	k := fixtureKey(t)
	cert, err := wire.NewTLSCertificate(k, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.VerifyTLSCertificate(cert.Certificate, k.Public().PeerID(), now, 16<<10); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.VerifyTLSCertificate(cert.Certificate, "", now.AddDate(2, 0, 0), 16<<10); err == nil {
		t.Fatal("expired certificate accepted")
	}
	if _, err := wire.VerifyTLSCertificate(append(cert.Certificate, cert.Certificate...), "", now, 16<<10); err == nil {
		t.Fatal("multiple certificates accepted")
	}
	cert.Certificate[0][len(cert.Certificate[0])-1] ^= 1
	if _, err := wire.VerifyTLSCertificate(cert.Certificate, "", now, 16<<10); err == nil {
		t.Fatal("invalid self-signature accepted")
	}
}
func FuzzTLSKeyExtension(f *testing.F) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	cert, err := wire.NewTLSCertificate(fixtureKey(f), now)
	if err != nil {
		f.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		f.Fatal(err)
	}
	for _, ext := range parsed.Extensions {
		if ext.Id.String() == "1.3.6.1.4.1.53594.1.1" {
			f.Add(ext.Value)
		}
	}
	f.Add([]byte{0x30, 0})
	f.Add(cert.Certificate[0])
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = wire.ParseTLSKeyExtension(b, 16<<10)
		_, _ = wire.VerifyTLSCertificate([][]byte{b}, "", now, 16<<10)
	})
}
