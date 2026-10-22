// Package qpath selects /nu/qpath/1 on raw circuit-v2 streams and hands framed
// packets to magicsock. It does not manage reservations, sessions or home relays.
package qpath

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const Protocol = "/nu/qpath/1"

// Dial requires an existing relay connection/reservation. On success the socket
// owns the circuit; canceling ctx afterward does not kill the packet path.
// The returned address is LOCAL ONLY: pass it to Host.Dial or Sessions.SetPeer,
// never advertise/cache it in peer records or share links.
func Dial(ctx context.Context, socket *magicsock.Conn, client *circuit.Client, relay, peer wire.PeerID) (wire.Multiaddr, *magicsock.Relay, error) {
	if socket == nil || client == nil {
		return wire.Multiaddr{}, nil, magicsock.ErrPeer
	}
	stream, err := client.Connect(ctx, relay, peer)
	if err != nil {
		return wire.Multiaddr{}, nil, err
	}
	return attach(ctx, socket, stream, peer, true)
}

// Accept consumes ONE already-admitted circuit, and owns it on both success and
// failure. The caller bounds concurrency and applies peer/relay admission before
// this call. stream.Peer is relay-asserted, not end-to-end identity; Host's socket
// policy checks the inner libp2p TLS identity before any application admission.
// This function never replaces a live relay leg based on untrusted metadata.
func Accept(ctx context.Context, socket *magicsock.Conn, stream *circuit.Circuit) (wire.Multiaddr, *magicsock.Relay, error) {
	if stream == nil {
		return wire.Multiaddr{}, nil, magicsock.ErrPeer
	}
	if socket == nil {
		stream.Reset()
		return wire.Multiaddr{}, nil, magicsock.ErrPeer
	}
	return attach(ctx, socket, stream, stream.Peer, false)
}
func attach(ctx context.Context, socket *magicsock.Conn, stream *circuit.Circuit, peer wire.PeerID, dial bool) (wire.Multiaddr, *magicsock.Relay, error) {
	success := false
	defer func() {
		if !success {
			stream.Reset()
		}
	}()
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, _ := setup.Deadline()
	_ = stream.SetDeadline(deadline)
	stop := context.AfterFunc(setup, stream.Reset)
	defer stop()
	if dial {
		selectStream, err := wire.NewLazySelect(stream, Protocol)
		if err != nil {
			return wire.Multiaddr{}, nil, err
		}
		if err = selectStream.Confirm(); err != nil {
			return wire.Multiaddr{}, nil, err
		}
	} else {
		if _, err := wire.Negotiate(stream, func(p string) bool { return p == Protocol }); err != nil {
			return wire.Multiaddr{}, nil, err
		}
	}
	if !stop() || setup.Err() != nil {
		return wire.Multiaddr{}, nil, setup.Err()
	}
	_ = stream.SetDeadline(time.Time{})
	addr, err := socket.AddPeer(peer)
	if err != nil {
		return wire.Multiaddr{}, nil, err
	}
	multiaddr, err := LocalAddress(peer, addr)
	if err != nil {
		return wire.Multiaddr{}, nil, err
	}
	path, err := socket.AttachRelay(peer, stream)
	if err != nil {
		return wire.Multiaddr{}, nil, err
	}
	success = true
	return multiaddr, path, nil
}

func LocalAddress(peer wire.PeerID, addr netip.AddrPort) (wire.Multiaddr, error) {
	if _, err := wire.PeerIDFromBytes(peer.Bytes()); err != nil {
		return wire.Multiaddr{}, err
	}
	if !magicsock.IsVirtual(addr) {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	family := "ip6"
	if addr.Addr().Is4() {
		family = "ip4"
	}
	return wire.ParseMultiaddr(fmt.Sprintf("/%s/%s/udp/%d/quic-v1/p2p/%s", family, addr.Addr(), addr.Port(), peer), 1024)
}

// UpgradeDatagram negotiates the Nu datagram relay extension for an existing
// stream leg (the circuit dialer calls it; the relay offers the other half to the
// target). The stream leg stays attached as the fallback. On any failure the
// stream leg is untouched and the error explains why (ErrDatagramUnsupported
// covers unsupported relays/peers, insufficient budget and refused admission).
func UpgradeDatagram(ctx context.Context, client *circuit.Client, leg *magicsock.Relay, relay, peer wire.PeerID) (*circuit.DatagramLeg, error) {
	if client == nil || leg == nil {
		return nil, magicsock.ErrPeer
	}
	if leg.HasPacketLeg() {
		return nil, magicsock.ErrLegExists
	}
	dl, err := client.UpgradeDatagram(ctx, relay, peer)
	if err != nil {
		return nil, err
	}
	if err := leg.AttachPacketLeg(dl); err != nil {
		dl.Close()
		return nil, err
	}
	return dl, nil
}

// AttachDatagram binds a relay-offered datagram leg (circuit.Client.AcceptDatagram)
// to the live stream leg for the same peer and relay; mismatches close the offer.
func AttachDatagram(leg *magicsock.Relay, dl *circuit.DatagramLeg, relay, peer wire.PeerID) error {
	if dl == nil {
		return magicsock.ErrPeer
	}
	if leg == nil || dl.Relay != relay || dl.Peer != peer {
		dl.Close()
		return magicsock.ErrPeer
	}
	if err := leg.AttachPacketLeg(dl); err != nil {
		dl.Close()
		return err
	}
	return nil
}
