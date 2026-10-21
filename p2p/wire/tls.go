package wire

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"time"
)

const TLSALPN = "libp2p"
const tlsSignaturePrefix = "libp2p-tls-handshake:"

var tlsExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 53594, 1, 1}

type tlsSignedKey struct{ PublicKey, Signature []byte }

// NewTLSCertificate uses an unrelated ephemeral P-256 certificate key, bound to
// the node identity by the libp2p signed-key extension.
func NewTLSCertificate(key *NodeKey, now time.Time) (tls.Certificate, error) {
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	spki, err := x509.MarshalPKIXPublicKey(&certKey.PublicKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	ext, err := asn1.Marshal(tlsSignedKey{key.Public().Marshal(), key.Sign(append([]byte(tlsSignaturePrefix), spki...))})
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{SerialNumber: serial.String()}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), ExtraExtensions: []pkix.Extension{{Id: tlsExtensionOID, Value: ext}}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &certKey.PublicKey, certKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: certKey}, nil
}

// ParseTLSKeyExtension decodes bounded DER SignedKey, rejecting trailing data.
// The returned signature is untrusted until checked against the certificate SPKI.
func ParseTLSKeyExtension(b []byte, maxBytes int) (PublicKey, []byte, error) {
	if maxBytes < 0 || len(b) > maxBytes {
		return PublicKey{}, nil, ErrLimit
	}
	var sk tlsSignedKey
	rest, err := asn1.Unmarshal(b, &sk)
	if err != nil {
		return PublicKey{}, nil, err
	}
	if len(rest) != 0 {
		return PublicKey{}, nil, ErrFormat
	}
	canonical, err := asn1.Marshal(sk)
	if err != nil || !bytes.Equal(canonical, b) {
		return PublicKey{}, nil, ErrFormat
	}
	key, err := ParsePublicKey(sk.PublicKey, MaxKeyBytes)
	if err != nil {
		return PublicKey{}, nil, err
	}
	return key, sk.Signature, nil
}

// VerifyTLSCertificate implements libp2p TLS authentication, not Web PKI: one
// current, self-signed certificate, only understood critical extensions, and a
// valid identity signature binding its exact SPKI. expected may be empty only
// when accepting a peer whose identity is not yet known.
func VerifyTLSCertificate(chain [][]byte, expected PeerID, now time.Time, maxBytes int) (PublicKey, error) {
	if len(chain) != 1 {
		return PublicKey{}, ErrFormat
	}
	if maxBytes < 0 || len(chain[0]) > maxBytes {
		return PublicKey{}, ErrLimit
	}
	cert, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return PublicKey{}, err
	}
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return PublicKey{}, fmt.Errorf("wire: expired or not-yet-valid TLS certificate")
	}
	for _, oid := range cert.UnhandledCriticalExtensions {
		if !oid.Equal(tlsExtensionOID) {
			return PublicKey{}, fmt.Errorf("wire: unknown critical TLS extension")
		}
	}
	// CheckSignatureFrom imposes CA constraints inappropriate for self-signed
	// endpoint certificates. CheckSignature verifies the actual self-signature.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return PublicKey{}, err
	}
	var extension []byte
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(tlsExtensionOID) {
			if extension != nil {
				return PublicKey{}, ErrFormat
			}
			extension = ext.Value
		}
	}
	if extension == nil {
		return PublicKey{}, fmt.Errorf("wire: missing libp2p TLS key extension")
	}
	key, sig, err := ParseTLSKeyExtension(extension, maxBytes)
	if err != nil {
		return PublicKey{}, err
	}
	if err := key.Verify(append([]byte(tlsSignaturePrefix), cert.RawSubjectPublicKeyInfo...), sig); err != nil {
		return PublicKey{}, err
	}
	if expected != "" && key.PeerID() != expected {
		return PublicKey{}, fmt.Errorf("wire: PeerId mismatch: expected %s, got %s", expected, key.PeerID())
	}
	return key, nil
}
