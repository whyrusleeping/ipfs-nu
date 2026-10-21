// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/portmapper/pcp.go; see NOTICE.
// Nu adds PEER, IPv6, strict response correlation and explicit parser limits.
package portmap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
)

const maxPacket = 1100

var be = binary.BigEndian

type pcpRequest struct {
	Local, External, Remote netip.AddrPort
	Nonce                   [12]byte
	Lifetime                uint32
}

func (r pcpRequest) opcode() byte {
	if r.Remote.IsValid() {
		return 2
	}
	return 1
}
func pcpPacket(r pcpRequest) []byte {
	n := 60
	if r.Remote.IsValid() {
		n = 80
	}
	b := make([]byte, n)
	b[0], b[1] = 2, r.opcode()
	be.PutUint32(b[4:8], r.Lifetime)
	ip := r.Local.Addr().As16()
	copy(b[8:24], ip[:])
	copy(b[24:36], r.Nonce[:])
	b[36] = 17 // UDP
	be.PutUint16(b[40:42], r.Local.Port())
	be.PutUint16(b[42:44], r.External.Port())
	ip = r.External.Addr().As16()
	copy(b[44:60], ip[:])
	if r.Remote.IsValid() {
		be.PutUint16(b[60:62], r.Remote.Port())
		ip = r.Remote.Addr().As16()
		copy(b[64:80], ip[:])
	}
	return b
}
func pcpAnnounce(ip netip.Addr) []byte {
	b := make([]byte, 24)
	b[0] = 2
	a := ip.As16()
	copy(b[8:], a[:])
	return b
}

type pcpResponse struct {
	Epoch, Lifetime uint32
	External        netip.AddrPort
}

func parsePCP(b []byte, req []byte, maxBytes int) (r pcpResponse, err error) {
	if len(b) > maxBytes || len(b) < 24 || len(req) < 24 || b[0] != 2 || b[1] != req[1]|0x80 {
		return r, ErrProtocol
	}
	r.Epoch = be.Uint32(b[8:12])
	r.Lifetime = be.Uint32(b[4:8])
	if b[3] != 0 {
		return r, fmt.Errorf("%w: PCP result %d", ErrProtocol, b[3])
	}
	n := 24
	if req[1] != 0 {
		n = 60
		if req[1] == 2 {
			n = 80
		}
		if len(b) < n || len(req) < n || !bytes.Equal(b[24:37], req[24:37]) || !bytes.Equal(b[40:42], req[40:42]) {
			return r, ErrProtocol
		}
		if n == 80 && (!bytes.Equal(b[60:62], req[60:62]) || !bytes.Equal(b[64:80], req[64:80])) {
			return r, ErrProtocol
		}
		r.External = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[44:60])).Unmap(), be.Uint16(b[42:44]))
	}
	// Unknown optional options can be ignored, mandatory options cannot.
	for off := n; off < len(b); {
		if len(b)-off < 4 {
			return r, ErrProtocol
		}
		size := int(be.Uint16(b[off+2 : off+4]))
		padded := (size + 3) &^ 3
		if b[off] < 128 || padded > len(b)-off-4 {
			return r, ErrProtocol
		}
		off += 4 + padded
	}
	return r, nil
}
