package circuit

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Nu datagram relay extension (D11c §5). A client with a live admitted circuit to
// a peer asks its relay to carry that pair's inner QUIC packets as outer QUIC
// DATAGRAM frames instead of circuit-stream bytes. The relay offers the same to
// the target through its reservation connection, proves the 1202-byte payload
// budget on BOTH outer legs with channel-0 probes, and only then signals READY.
// Standard circuit streams stay in place as the fallback for every packet the
// datagram leg cannot carry. Non-Nu peers never see these protocols.
const (
	DatagramHopProtocol  = "/nu/relay/datagram/1/hop"
	DatagramStopProtocol = "/nu/relay/datagram/1/stop"
	// DatagramPacketSize is the fixed inner packet budget (magicsock.PacketSize).
	DatagramPacketSize = 1200
	// DatagramPayload is the outer DATAGRAM payload needed per inner packet:
	// channel header plus one complete 1200-byte inner packet. A 1200-byte outer
	// packet budget (1163 payload bytes) cannot carry it; 1280 (1243) can.
	DatagramPayload   = 2 + DatagramPacketSize
	datagramVersion   = 1
	maxDatagramMsg    = 1 + 1 + 2 + 2 + 1 + 64
	dgRequest         = 1
	dgAccept          = 2
	dgReady           = 3
	dgFail            = 4
	datagramQueue     = 64
	datagramProbeTrys = 3
)

var ErrDatagramUnsupported = errors.New("circuit: datagram relaying unavailable (unsupported, insufficient budget or not admitted)")

type datagramMessage struct {
	kind    uint8
	channel uint16
	peer    wire.PeerID
	status  Status
}

// Control messages are `len:u8 | version=1:u8 | type:u8 | fields`:
//
//	REQUEST(1): channel:u16 (0 from a client; relay-allocated toward a target), peer_len:u8, peer bytes
//	ACCEPT(2):  channel:u16 (relay-allocated for the receiver of this message)
//	READY(3):   no fields; both outer legs carry 1202-byte datagrams
//	FAIL(4):    status:u16 (circuit Status codes)
//
// Any other encoding terminates the control stream, which ends the leg.
func encodeDatagramMessage(m datagramMessage) ([]byte, error) {
	b := []byte{0, datagramVersion, m.kind}
	switch m.kind {
	case dgRequest:
		if m.peer == "" || len(m.peer.Bytes()) > 64 {
			return nil, wire.ErrFormat
		}
		b = binary.BigEndian.AppendUint16(b, m.channel)
		b = append(b, byte(len(m.peer.Bytes())))
		b = append(b, m.peer.Bytes()...)
	case dgAccept:
		if m.channel == 0 {
			return nil, wire.ErrFormat
		}
		b = binary.BigEndian.AppendUint16(b, m.channel)
	case dgReady:
	case dgFail:
		if m.status == 0 || m.status > 0xffff {
			return nil, wire.ErrFormat
		}
		b = binary.BigEndian.AppendUint16(b, uint16(m.status))
	default:
		return nil, wire.ErrFormat
	}
	b[0] = byte(len(b) - 1)
	return b, nil
}
func decodeDatagramMessage(b []byte) (datagramMessage, error) {
	var m datagramMessage
	if len(b) < 2 || len(b) > maxDatagramMsg-1 || b[0] != datagramVersion {
		return m, wire.ErrFormat
	}
	m.kind = b[1]
	body := b[2:]
	switch m.kind {
	case dgRequest:
		if len(body) < 3 {
			return m, wire.ErrFormat
		}
		m.channel = binary.BigEndian.Uint16(body)
		n := int(body[2])
		if n == 0 || n > 64 || len(body) != 3+n {
			return m, wire.ErrFormat
		}
		id, err := wire.PeerIDFromBytes(body[3:])
		if err != nil {
			return m, err
		}
		m.peer = id
	case dgAccept:
		if len(body) != 2 {
			return m, wire.ErrFormat
		}
		m.channel = binary.BigEndian.Uint16(body)
		if m.channel == 0 {
			return m, wire.ErrFormat
		}
	case dgReady:
		if len(body) != 0 {
			return m, wire.ErrFormat
		}
	case dgFail:
		if len(body) != 2 {
			return m, wire.ErrFormat
		}
		m.status = Status(binary.BigEndian.Uint16(body))
		if m.status == 0 {
			return m, wire.ErrFormat
		}
	default:
		return m, wire.ErrFormat
	}
	return m, nil
}
func readDatagramMessage(r io.Reader) (datagramMessage, error) {
	var n [1]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return datagramMessage{}, err
	}
	if n[0] < 2 || int(n[0]) > maxDatagramMsg-1 {
		return datagramMessage{}, wire.ErrFormat
	}
	b := make([]byte, n[0])
	if _, err := io.ReadFull(r, b); err != nil {
		return datagramMessage{}, err
	}
	return decodeDatagramMessage(b)
}
func writeDatagramMessage(w io.Writer, m datagramMessage) error {
	b, err := encodeDatagramMessage(m)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// validInnerPacket applies the qpath frame rule: 1..1200 bytes, QUIC fixed bit.
func validInnerPacket(b []byte) bool {
	return len(b) >= 1 && len(b) <= DatagramPacketSize && b[0]&0x40 != 0
}
func pairKey(a, b wire.PeerID) [2]wire.PeerID {
	if a < b {
		return [2]wire.PeerID{a, b}
	}
	return [2]wire.PeerID{b, a}
}
