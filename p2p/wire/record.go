package wire

import (
	"bytes"
	"io"

	"github.com/whyrusleeping/nu/core/varint"
)

const PeerRecordDomain = "libp2p-peer-record"

// Envelope is a verified RFC 0002 signed envelope when returned by OpenEnvelope.
// Codec is application-defined bytes, not necessarily a varint (peer record is 03 01).
type Envelope struct {
	PublicKey PublicKey
	Codec     []byte
	Payload   []byte
	Signature []byte
}

func envelopeMessage(domain string, codec, payload []byte) []byte {
	var b []byte
	for _, part := range [][]byte{[]byte(domain), codec, payload} {
		b = varint.Append(b, uint64(len(part)))
		b = append(b, part...)
	}
	return b
}
func SealEnvelope(k *NodeKey, domain string, codec, payload []byte, maxBytes int) ([]byte, error) {
	if domain == "" {
		return nil, ErrFormat
	}
	if maxBytes < 0 || len(payload) > maxBytes || len(codec) > maxBytes || len(domain) > maxBytes {
		return nil, ErrLimit
	}
	sig := k.Sign(envelopeMessage(domain, codec, payload))
	b := AppendBytes(nil, 1, k.Public().Marshal())
	if len(codec) > 0 {
		b = AppendBytes(b, 2, codec)
	}
	if len(payload) > 0 {
		b = AppendBytes(b, 3, payload)
	}
	b = AppendBytes(b, 5, sig)
	if len(b) > maxBytes {
		return nil, ErrLimit
	}
	return b, nil
}

// OpenEnvelope checks the domain-separated signature before exposing any payload.
func OpenEnvelope(b []byte, domain string, maxBytes int) (Envelope, error) {
	var e Envelope
	if domain == "" {
		return e, ErrFormat
	}
	if len(domain) > maxBytes {
		return e, ErrLimit
	}
	r, err := NewProtoReader(b, maxBytes)
	if err != nil {
		return e, err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Envelope{}, err
		}
		switch f.Number {
		case 1:
			if f.Type != 2 {
				return Envelope{}, ErrFormat
			}
			e.PublicKey, err = ParsePublicKey(f.Bytes, MaxKeyBytes)
			if err != nil {
				return Envelope{}, err
			}
		case 2, 3, 5:
			if f.Type != 2 {
				return Envelope{}, ErrFormat
			}
			switch f.Number {
			case 2:
				e.Codec = bytes.Clone(f.Bytes)
			case 3:
				e.Payload = bytes.Clone(f.Bytes)
			case 5:
				e.Signature = bytes.Clone(f.Bytes)
			}
		}
	}
	if err := e.PublicKey.Verify(envelopeMessage(domain, e.Codec, e.Payload), e.Signature); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

type PeerRecord struct {
	PeerID    PeerID
	Seq       uint64
	Addresses []Multiaddr
}

// RecordLimits bounds both the outer envelope and nested address allocations.
// Callers must supply positive MaxBytes and MaxAddressBytes; zero MaxAddresses
// admits only records without addresses.
type RecordLimits struct{ MaxBytes, MaxAddresses, MaxAddressBytes int }

func (r PeerRecord) Marshal(limits RecordLimits) ([]byte, error) {
	if limits.MaxAddresses < 0 || len(r.Addresses) > limits.MaxAddresses {
		return nil, ErrLimit
	}
	if _, err := PeerIDFromBytes(r.PeerID.Bytes()); err != nil {
		return nil, err
	}
	b := AppendBytes(nil, 1, r.PeerID.Bytes())
	if r.Seq != 0 {
		b = AppendUint(b, 2, r.Seq)
	}
	for _, a := range r.Addresses {
		if _, err := MultiaddrFromBytes(a.raw, limits.MaxAddressBytes); err != nil {
			return nil, err
		}
		b = AppendBytes(b, 3, AppendBytes(nil, 1, a.raw))
		if len(b) > limits.MaxBytes {
			return nil, ErrLimit
		}
	}
	if len(b) > limits.MaxBytes {
		return nil, ErrLimit
	}
	return b, nil
}
func ParsePeerRecord(b []byte, limits RecordLimits) (PeerRecord, error) {
	var record PeerRecord
	if limits.MaxAddresses < 0 || limits.MaxAddressBytes < 0 {
		return record, ErrLimit
	}
	r, err := NewProtoReader(b, limits.MaxBytes)
	if err != nil {
		return record, err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return PeerRecord{}, err
		}
		switch f.Number {
		case 1:
			if f.Type != 2 {
				return PeerRecord{}, ErrFormat
			}
			record.PeerID, err = PeerIDFromBytes(f.Bytes)
			if err != nil {
				return PeerRecord{}, err
			}
		case 2:
			if f.Type != 0 {
				return PeerRecord{}, ErrFormat
			}
			record.Seq = f.Uint
		case 3:
			if f.Type != 2 {
				return PeerRecord{}, ErrFormat
			}
			if len(record.Addresses) >= limits.MaxAddresses {
				return PeerRecord{}, ErrLimit
			}
			nested, err := NewProtoReader(f.Bytes, limits.MaxBytes)
			if err != nil {
				return PeerRecord{}, err
			}
			var addr Multiaddr
			found := false
			for {
				info, err := nested.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return PeerRecord{}, err
				}
				if info.Number != 1 {
					continue
				}
				if info.Type != 2 {
					return PeerRecord{}, ErrFormat
				}
				addr, err = MultiaddrFromBytes(info.Bytes, limits.MaxAddressBytes)
				if err != nil {
					return PeerRecord{}, err
				}
				found = true
			}
			if !found {
				return PeerRecord{}, ErrFormat
			}
			record.Addresses = append(record.Addresses, addr)
		}
	}
	if record.PeerID == "" {
		return PeerRecord{}, ErrFormat
	}
	return record, nil
}
func (r PeerRecord) Seal(k *NodeKey, limits RecordLimits) ([]byte, error) {
	if r.PeerID != k.Public().PeerID() {
		return nil, ErrFormat
	}
	payload, err := r.Marshal(limits)
	if err != nil {
		return nil, err
	}
	return SealEnvelope(k, PeerRecordDomain, []byte{3, 1}, payload, limits.MaxBytes)
}

// OpenPeerRecord verifies the envelope and binds its signer to the record PeerID.
// Sequence/replay policy is the consuming peer store's responsibility.
func OpenPeerRecord(b []byte, limits RecordLimits) (PeerRecord, error) {
	e, err := OpenEnvelope(b, PeerRecordDomain, limits.MaxBytes)
	if err != nil {
		return PeerRecord{}, err
	}
	if !bytes.Equal(e.Codec, []byte{3, 1}) {
		return PeerRecord{}, ErrFormat
	}
	r, err := ParsePeerRecord(e.Payload, limits)
	if err != nil {
		return PeerRecord{}, err
	}
	if r.PeerID != e.PublicKey.PeerID() {
		return PeerRecord{}, ErrSignature
	}
	return r, nil
}
