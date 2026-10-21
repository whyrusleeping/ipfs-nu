// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/portmapper/portmapper.go; see NOTICE.
package portmap

import (
	"fmt"
	"net/netip"
)

func pmpPacket(local, external uint16, lifetime uint32) []byte {
	b := make([]byte, 12)
	b[1] = 1
	be.PutUint16(b[4:6], local)
	be.PutUint16(b[6:8], external)
	be.PutUint32(b[8:12], lifetime)
	return b
}

type pmpResponse struct {
	Epoch, Lifetime uint32
	Local, External uint16
	Public          netip.Addr
}

func parsePMP(b []byte, opcode byte, maxBytes int) (r pmpResponse, err error) {
	if len(b) > maxBytes || len(b) < 8 || b[0] != 0 || b[1] != opcode|128 {
		return r, ErrProtocol
	}
	r.Epoch = be.Uint32(b[4:8])
	if code := be.Uint16(b[2:4]); code != 0 {
		return r, fmt.Errorf("%w: NAT-PMP result %d", ErrProtocol, code)
	}
	switch opcode {
	case 0:
		if len(b) != 12 {
			return r, ErrProtocol
		}
		r.Public = netip.AddrFrom4([4]byte(b[8:12]))
	case 1:
		if len(b) != 16 {
			return r, ErrProtocol
		}
		r.Local = be.Uint16(b[8:10])
		r.External = be.Uint16(b[10:12])
		r.Lifetime = be.Uint32(b[12:16])
	default:
		return r, ErrProtocol
	}
	return r, nil
}
