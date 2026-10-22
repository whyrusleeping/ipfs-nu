package netcheck

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Observe reports this socket's endpoint at one observer. The caller must be the
// sole reader for the duration (e.g. before handing it to magicsock). Ownership
// is retained and the read deadline is reset on return. A report is an address
// assertion, not reachability or permission to probe that address.
func Observe(ctx context.Context, pc net.PacketConn, observer netip.AddrPort) (netip.AddrPort, error) {
	if !observer.IsValid() || observer.Port() == 0 {
		return netip.AddrPort{}, ErrNoObservers
	}
	p := packet{Type: typeRequest}
	if _, err := rand.Read(p.TxID[:]); err != nil {
		return netip.AddrPort{}, err
	}
	b := p.marshal()
	defer pc.SetReadDeadline(time.Time{})
	buf := make([]byte, PacketSize+1)
	for range 3 {
		if err := ctx.Err(); err != nil {
			return netip.AddrPort{}, err
		}
		if _, err := pc.WriteTo(b[:], net.UDPAddrFromAddrPort(observer)); err != nil {
			return netip.AddrPort{}, err
		}
		until := time.Now().Add(250 * time.Millisecond)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
			until = deadline
		}
		_ = pc.SetReadDeadline(until)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				break
			}
			u, ok := from.(*net.UDPAddr)
			if !ok || netip.AddrPortFrom(u.AddrPort().Addr().Unmap(), u.AddrPort().Port()) != observer {
				continue
			}
			r, err := parsePacket(buf[:n])
			if err == nil && r.Type == typeResponse && r.TxID == p.TxID && r.Role == RolePrimary {
				return r.Observed, nil
			}
		}
	}
	return netip.AddrPort{}, fmt.Errorf("netcheck: observer did not answer")
}
