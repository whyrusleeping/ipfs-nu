package autonat

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const (
	DialProtocol     = "/libp2p/autonat/2/dial-request"
	DialBackProtocol = "/libp2p/autonat/2/dial-back"
	maxMessage       = 8192
	maxAddresses     = 50
	maxData          = 4096
	minDialData      = 30_000
	maxDialData      = 100_000

	dialRequest  = 1
	dialResponse = 2
	dataRequest  = 3
	dataResponse = 4

	responseInternal = 0
	responseRejected = 100
	responseRefused  = 101
	responseOK       = 200
	dialError        = 100
	dialBackError    = 101
	dialOK           = 200
)

// message is the bounded AutoNAT v2 Message oneof, not a Nu-specific encoding.
type message struct {
	kind               int
	addrs              [][]byte
	nonce              uint64
	index              uint32
	status, dialStatus uint32
	numBytes           uint64
	data               []byte
}

func fixed64(b []byte, field byte, n uint64) []byte {
	b = append(b, field<<3|1)
	return binary.LittleEndian.AppendUint64(b, n)
}
func (m message) marshal() []byte {
	var b []byte
	switch m.kind {
	case dialRequest:
		for _, a := range m.addrs {
			b = wire.AppendBytes(b, 1, a)
		}
		b = fixed64(b, 2, m.nonce)
	case dialResponse:
		b = wire.AppendUint(b, 1, uint64(m.status))
		b = wire.AppendUint(b, 2, uint64(m.index))
		b = wire.AppendUint(b, 3, uint64(m.dialStatus))
	case dataRequest:
		b = wire.AppendUint(b, 1, uint64(m.index))
		b = wire.AppendUint(b, 2, m.numBytes)
	case dataResponse:
		b = wire.AppendBytes(b, 1, m.data)
	}
	return wire.AppendBytes(nil, m.kind, b)
}
func parseMessage(b []byte) (message, error) {
	m := message{}
	r, err := wire.NewProtoReader(b, maxMessage)
	if err != nil {
		return m, err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return message{}, err
		}
		if f.Number < dialRequest || f.Number > dataResponse {
			continue
		}
		if f.Type != 2 || m.kind != 0 {
			return message{}, wire.ErrFormat
		}
		m.kind = f.Number
		if err := m.parseBody(f.Bytes); err != nil {
			return message{}, err
		}
	}
	if m.kind == 0 {
		return message{}, wire.ErrFormat
	}
	return m, nil
}
func (m *message) parseBody(b []byte) error {
	r, err := wire.NewProtoReader(b, maxMessage)
	if err != nil {
		return err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch m.kind {
		case dialRequest:
			switch f.Number {
			case 1:
				if f.Type != 2 {
					return wire.ErrFormat
				}
				if len(m.addrs) >= maxAddresses || len(f.Bytes) > 1024 {
					return wire.ErrLimit
				}
				m.addrs = append(m.addrs, f.Bytes)
			case 2:
				if f.Type != 1 {
					return wire.ErrFormat
				}
				m.nonce = binary.LittleEndian.Uint64(f.Bytes)
			}
		case dialResponse:
			if f.Number >= 1 && f.Number <= 3 {
				if f.Type != 0 || f.Uint > math.MaxUint32 {
					return wire.ErrFormat
				}
				switch f.Number {
				case 1:
					m.status = uint32(f.Uint)
				case 2:
					m.index = uint32(f.Uint)
				case 3:
					m.dialStatus = uint32(f.Uint)
				}
			}
		case dataRequest:
			if f.Number == 1 || f.Number == 2 {
				if f.Type != 0 {
					return wire.ErrFormat
				}
				if f.Number == 1 {
					if f.Uint > math.MaxUint32 {
						return wire.ErrFormat
					}
					m.index = uint32(f.Uint)
				} else {
					m.numBytes = f.Uint
				}
			}
		case dataResponse:
			if f.Number == 1 {
				if f.Type != 2 {
					return wire.ErrFormat
				}
				if len(f.Bytes) > maxData {
					return wire.ErrLimit
				}
				m.data = f.Bytes
			}
		}
	}
}
func readMessage(r io.Reader) (message, error) {
	b, err := wire.ReadMessage(r, maxMessage)
	if err != nil {
		return message{}, err
	}
	return parseMessage(b)
}
func writeMessage(w io.Writer, m message) error {
	return wire.WriteMessage(w, m.marshal(), maxMessage)
}

// DialBack and DialBackResponse are NOT wrapped in Message.
func parseDialBack(b []byte) (uint64, error) {
	r, err := wire.NewProtoReader(b, 1024)
	if err != nil {
		return 0, err
	}
	var nonce uint64
	for {
		f, err := r.Next()
		if err == io.EOF {
			return nonce, nil
		}
		if err != nil {
			return 0, err
		}
		if f.Number == 1 {
			if f.Type != 1 {
				return 0, wire.ErrFormat
			}
			nonce = binary.LittleEndian.Uint64(f.Bytes)
		}
	}
}
func parseDialBackResponse(b []byte) error {
	r, err := wire.NewProtoReader(b, 1024)
	if err != nil {
		return err
	}
	for {
		f, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if f.Number == 1 && (f.Type != 0 || f.Uint != 0) {
			return wire.ErrFormat
		}
	}
}
