package proto

import (
	"bytes"
	"io"
	"unicode/utf8"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const (
	IdentifyID     = "/ipfs/id/1.0.0"
	IdentifyPushID = "/ipfs/id/push/1.0.0"
	PingID         = "/ipfs/ping/1.0.0"
)

type IdentifyLimits struct{ MaxBytes, MaxAddresses, MaxAddressBytes, MaxProtocols, MaxStringBytes int }

func DefaultIdentifyLimits() IdentifyLimits {
	return IdentifyLimits{MaxBytes: 64 << 10, MaxAddresses: 128, MaxAddressBytes: 1024, MaxProtocols: 1024, MaxStringBytes: 1024}
}

type IdentifyMessage struct {
	PublicKey                     []byte
	ListenAddrs                   []wire.Multiaddr
	Protocols                     []string
	ObservedAddr                  *wire.Multiaddr
	ProtocolVersion, AgentVersion string
	SignedPeerRecord              []byte
}

func ParseIdentify(b []byte, limits IdentifyLimits) (IdentifyMessage, error) {
	var m IdentifyMessage
	if limits.MaxAddresses < 0 || limits.MaxAddressBytes < 0 || limits.MaxProtocols < 0 || limits.MaxStringBytes < 0 {
		return m, wire.ErrLimit
	}
	r, err := wire.NewProtoReader(b, limits.MaxBytes)
	if err != nil {
		return m, err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return IdentifyMessage{}, err
		}
		if f.Number != 1 && f.Number != 2 && f.Number != 3 && f.Number != 4 && f.Number != 5 && f.Number != 6 && f.Number != 8 {
			continue
		}
		if f.Type != 2 {
			return IdentifyMessage{}, wire.ErrFormat
		}
		switch f.Number {
		case 1:
			if len(f.Bytes) > 0 {
				if _, err := wire.ParsePublicKey(f.Bytes, wire.MaxKeyBytes); err != nil {
					return IdentifyMessage{}, err
				}
			}
			m.PublicKey = bytes.Clone(f.Bytes)
		case 2:
			if len(m.ListenAddrs) >= limits.MaxAddresses {
				return IdentifyMessage{}, wire.ErrLimit
			}
			a, err := wire.MultiaddrFromBytes(f.Bytes, limits.MaxAddressBytes)
			if err != nil {
				return IdentifyMessage{}, err
			}
			m.ListenAddrs = append(m.ListenAddrs, a)
		case 3, 5, 6:
			if len(f.Bytes) > limits.MaxStringBytes {
				return IdentifyMessage{}, wire.ErrLimit
			}
			if !utf8.Valid(f.Bytes) {
				return IdentifyMessage{}, wire.ErrFormat
			}
			text := string(f.Bytes)
			switch f.Number {
			case 3:
				if len(m.Protocols) >= limits.MaxProtocols {
					return IdentifyMessage{}, wire.ErrLimit
				}
				if !wire.ValidProtocol(text) {
					return IdentifyMessage{}, wire.ErrFormat
				}
				m.Protocols = append(m.Protocols, text)
			case 5:
				m.ProtocolVersion = text
			case 6:
				m.AgentVersion = text
			}
		case 4:
			if len(f.Bytes) == 0 {
				m.ObservedAddr = nil
				continue
			}
			a, err := wire.MultiaddrFromBytes(f.Bytes, limits.MaxAddressBytes)
			if err != nil {
				return IdentifyMessage{}, err
			}
			m.ObservedAddr = &a
		case 8:
			m.SignedPeerRecord = bytes.Clone(f.Bytes)
		}
	}
	return m, nil
}
func (m IdentifyMessage) Marshal(limits IdentifyLimits) ([]byte, error) {
	var b []byte
	if len(m.PublicKey) > 0 {
		b = wire.AppendBytes(b, 1, m.PublicKey)
	}
	for _, a := range m.ListenAddrs {
		b = wire.AppendBytes(b, 2, a.Bytes())
	}
	for _, p := range m.Protocols {
		b = wire.AppendBytes(b, 3, []byte(p))
	}
	if m.ObservedAddr != nil {
		b = wire.AppendBytes(b, 4, m.ObservedAddr.Bytes())
	}
	if m.ProtocolVersion != "" {
		b = wire.AppendBytes(b, 5, []byte(m.ProtocolVersion))
	}
	if m.AgentVersion != "" {
		b = wire.AppendBytes(b, 6, []byte(m.AgentVersion))
	}
	if len(m.SignedPeerRecord) > 0 {
		b = wire.AppendBytes(b, 8, m.SignedPeerRecord)
	}
	if _, err := ParseIdentify(b, limits); err != nil {
		return nil, err
	}
	return b, nil
}

// ReadIdentify accepts the deployed multi-part identify encoding: protobuf
// fragments merge by concatenation (last singular field wins, repeated fields
// append). At most ten 8-KiB parts and 64 KiB total are accepted, then clean EOF.
func ReadIdentify(r io.Reader, limits IdentifyLimits) (IdentifyMessage, error) {
	var all []byte
	for parts := 0; ; parts++ {
		maxBytes := min(8<<10, limits.MaxBytes-len(all))
		b, err := wire.ReadMessage(r, maxBytes)
		if err == io.EOF {
			if parts == 0 {
				return IdentifyMessage{}, io.ErrUnexpectedEOF
			}
			return ParseIdentify(all, limits)
		}
		if err != nil {
			return IdentifyMessage{}, err
		}
		if parts >= 10 {
			return IdentifyMessage{}, wire.ErrLimit
		}
		all = append(all, b...)
	}
}
func cloneMessage(m IdentifyMessage) IdentifyMessage {
	m.PublicKey = bytes.Clone(m.PublicKey)
	m.SignedPeerRecord = bytes.Clone(m.SignedPeerRecord)
	m.ListenAddrs = append([]wire.Multiaddr(nil), m.ListenAddrs...)
	m.Protocols = append([]string(nil), m.Protocols...)
	if m.ObservedAddr != nil {
		a := *m.ObservedAddr
		m.ObservedAddr = &a
	}
	return m
}
