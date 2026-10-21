// Package circuit implements libp2p circuit relay v2 control messages and raw
// circuit streams. A circuit is NOT an authenticated end-to-end host connection.
package circuit

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const (
	HopProtocol   = "/libp2p/circuit/relay/0.2.0/hop"
	StopProtocol  = "/libp2p/circuit/relay/0.2.0/stop"
	MaxMessage    = 4096
	MaxAddresses  = 16
	VoucherDomain = "libp2p-relay-rsvp"
)

type Status uint32

const (
	OK                 Status = 100
	ReservationRefused Status = 200
	ResourceLimit      Status = 201
	PermissionDenied   Status = 202
	ConnectionFailed   Status = 203
	NoReservation      Status = 204
	MalformedMessage   Status = 400
	UnexpectedMessage  Status = 401
)

func (s Status) Error() string { return fmt.Sprintf("circuit: relay status %d", s) }

// Limit values are seconds and bytes PER DIRECTION. Zero means unlimited.
type Limit struct {
	Duration uint32
	Data     uint64
}
type Reservation struct {
	Expires   time.Time
	Addresses []wire.Multiaddr
	Voucher   []byte
	Limit     Limit
}
type message struct {
	kind        uint64
	peer        wire.PeerID
	reservation *Reservation
	limit       Limit
	status      Status
}

func marshal(m message, stop bool) []byte {
	b := wire.AppendUint(nil, 1, m.kind)
	if m.peer != "" {
		b = wire.AppendBytes(b, 2, wire.AppendBytes(nil, 1, m.peer.Bytes()))
	}
	if !stop && m.reservation != nil {
		r := m.reservation
		v := wire.AppendUint(nil, 1, uint64(r.Expires.Unix()))
		for _, a := range r.Addresses {
			v = wire.AppendBytes(v, 2, a.Bytes())
		}
		if len(r.Voucher) > 0 {
			v = wire.AppendBytes(v, 3, r.Voucher)
		}
		b = wire.AppendBytes(b, 3, v)
	}
	lf, sf := 4, 5
	if stop {
		lf, sf = 3, 4
	}
	if m.limit != (Limit{}) {
		v := wire.AppendUint(nil, 1, uint64(m.limit.Duration))
		v = wire.AppendUint(v, 2, m.limit.Data)
		b = wire.AppendBytes(b, lf, v)
	}
	if m.status != 0 {
		b = wire.AppendUint(b, sf, uint64(m.status))
	}
	return b
}
func writeMessage(w io.Writer, m message, stop bool) error {
	return wire.WriteMessage(w, marshal(m, stop), MaxMessage)
}
func readMessage(r io.Reader, stop bool) (message, error) {
	b, err := wire.ReadMessage(r, MaxMessage)
	if err != nil {
		return message{}, err
	}
	return decode(b, stop, MaxMessage)
}

// decode bounds every nesting level, repeated address count, and singular field.
// Unknown fields are skipped for protobuf forward compatibility. Duplicate known
// singular fields and wrong wire types are rejected rather than last-wins.
func decode(b []byte, stop bool, maxBytes int) (message, error) {
	var m message
	r, err := wire.NewProtoReader(b, maxBytes)
	if err != nil {
		return m, err
	}
	lf, sf := 4, 5
	if stop {
		lf, sf = 3, 4
	}
	seen := map[int]bool{}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}
		known := f.Number == 1 || f.Number == 2 || f.Number == lf || f.Number == sf || (!stop && f.Number == 3)
		if !known {
			continue
		}
		if seen[f.Number] {
			return m, wire.ErrFormat
		}
		seen[f.Number] = true
		switch f.Number {
		case 1:
			if f.Type != 0 {
				return m, wire.ErrFormat
			}
			m.kind = f.Uint
		case 2:
			if f.Type != 2 {
				return m, wire.ErrFormat
			}
			m.peer, err = decodePeer(f.Bytes, maxBytes)
			if err != nil {
				return m, err
			}
		case sf:
			if f.Type != 0 || f.Uint > math.MaxUint32 {
				return m, wire.ErrFormat
			}
			m.status = Status(f.Uint)
		case lf:
			if f.Type != 2 {
				return m, wire.ErrFormat
			}
			m.limit, err = decodeLimit(f.Bytes, maxBytes)
			if err != nil {
				return m, err
			}
		case 3:
			if f.Type != 2 {
				return m, wire.ErrFormat
			}
			m.reservation, err = decodeReservation(f.Bytes, maxBytes)
			if err != nil {
				return m, err
			}
		}
	}
	if !seen[1] {
		return m, wire.ErrFormat
	}
	return m, nil
}
func decodePeer(b []byte, maxBytes int) (wire.PeerID, error) {
	r, err := wire.NewProtoReader(b, maxBytes)
	if err != nil {
		return "", err
	}
	var id wire.PeerID
	count := 0
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch f.Number {
		case 1:
			if f.Type != 2 || id != "" {
				return "", wire.ErrFormat
			}
			id, err = wire.PeerIDFromBytes(f.Bytes)
			if err != nil {
				return "", err
			}
		case 2:
			count++
			if f.Type != 2 {
				return "", wire.ErrFormat
			}
			if count > MaxAddresses || len(f.Bytes) > 1024 {
				return "", wire.ErrLimit
			}
		}
	}
	if id == "" {
		return "", wire.ErrFormat
	}
	return id, nil
}
func decodeLimit(b []byte, maxBytes int) (Limit, error) {
	var l Limit
	r, err := wire.NewProtoReader(b, maxBytes)
	if err != nil {
		return l, err
	}
	seen := map[int]bool{}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return l, err
		}
		if f.Number != 1 && f.Number != 2 {
			continue
		}
		if f.Type != 0 || seen[f.Number] {
			return l, wire.ErrFormat
		}
		seen[f.Number] = true
		if f.Number == 1 {
			if f.Uint > math.MaxUint32 {
				return l, wire.ErrFormat
			}
			l.Duration = uint32(f.Uint)
		} else {
			l.Data = f.Uint
		}
	}
	return l, nil
}
func decodeReservation(b []byte, maxBytes int) (*Reservation, error) {
	r, err := wire.NewProtoReader(b, maxBytes)
	if err != nil {
		return nil, err
	}
	out := &Reservation{}
	seen := map[int]bool{}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if f.Number == 1 || f.Number == 3 {
			if seen[f.Number] {
				return nil, wire.ErrFormat
			}
			seen[f.Number] = true
		}
		switch f.Number {
		case 1:
			if f.Type != 0 || f.Uint > math.MaxInt64 {
				return nil, wire.ErrFormat
			}
			out.Expires = time.Unix(int64(f.Uint), 0)
		case 2:
			if f.Type != 2 {
				return nil, wire.ErrFormat
			}
			if len(out.Addresses) >= MaxAddresses {
				return nil, wire.ErrLimit
			}
			a, err := wire.MultiaddrFromBytes(f.Bytes, 1024)
			if err != nil {
				return nil, err
			}
			out.Addresses = append(out.Addresses, a)
		case 3:
			if f.Type != 2 {
				return nil, wire.ErrFormat
			}
			out.Voucher = bytes.Clone(f.Bytes)
		}
	}
	if !seen[1] {
		return nil, wire.ErrFormat
	}
	return out, nil
}

// SealVoucher creates the canonical field-ordered payload in a signed envelope.
func SealVoucher(key *wire.NodeKey, peer wire.PeerID, expires time.Time) ([]byte, error) {
	if key == nil || expires.Unix() <= 0 {
		return nil, wire.ErrFormat
	}
	if _, err := wire.PeerIDFromBytes(peer.Bytes()); err != nil {
		return nil, err
	}
	return wire.SealEnvelope(key, VoucherDomain, []byte{3, 2}, voucherPayload(key.Public().PeerID(), peer, expires), MaxMessage)
}
func voucherPayload(relay, peer wire.PeerID, expires time.Time) []byte {
	b := wire.AppendBytes(nil, 1, relay.Bytes())
	b = wire.AppendBytes(b, 2, peer.Bytes())
	return wire.AppendUint(b, 3, uint64(expires.Unix()))
}

// VerifyVoucher binds signer, relay, reserving peer and expiration, and requires
// the spec's canonical payload. The voucher is evidence, not dial authorization.
func VerifyVoucher(b []byte, relay, peer wire.PeerID, expires time.Time) error {
	e, err := wire.OpenEnvelope(b, VoucherDomain, MaxMessage)
	if err != nil {
		return err
	}
	if e.PublicKey.PeerID() != relay || !bytes.Equal(e.Codec, []byte{3, 2}) || !bytes.Equal(e.Payload, voucherPayload(relay, peer, expires)) {
		return wire.ErrSignature
	}
	return nil
}
