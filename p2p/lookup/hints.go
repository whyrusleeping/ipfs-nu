package lookup

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var ErrPeerMismatch = errors.New("lookup: address PeerId does not match requested peer")

type Hint struct {
	Peer      wire.PeerID
	Addresses []wire.Multiaddr
}
type HintLimits struct{ MaxBytes, MaxPeers, MaxAddresses, MaxAddressBytes int }

func DefaultHintLimits() HintLimits { return HintLimits{8192, 16, 32, 1024} }

// NormalizeAddress binds a direct or circuit address to its destination PeerId.
// An omitted terminal /p2p is added; an existing conflicting destination is an
// error, never silently rewritten. Relay hops are not destination identities.
func NormalizeAddress(id wire.PeerID, a wire.Multiaddr) (wire.Multiaddr, error) {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return wire.Multiaddr{}, err
	}
	if a.Opaque() {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil || len(m) == 0 {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	parts := ma.Split(m)
	if parts[len(parts)-1].Protocol().Code == ma.P_P2P {
		got, err := wire.ParsePeerID(parts[len(parts)-1].Value())
		if err != nil {
			return wire.Multiaddr{}, err
		}
		if got != id {
			return wire.Multiaddr{}, fmt.Errorf("%w: expected %s, got %s", ErrPeerMismatch, id, got)
		}
		return a, nil
	}
	text, err := a.Text()
	if err != nil {
		return wire.Multiaddr{}, err
	}
	return wire.ParseMultiaddr(text+"/p2p/"+id.String(), 2048)
}
func AddressPeer(a wire.Multiaddr) (wire.PeerID, error) {
	if a.Opaque() {
		return "", wire.ErrFormat
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil || len(m) == 0 {
		return "", wire.ErrFormat
	}
	parts := ma.Split(m)
	last := parts[len(parts)-1]
	if last.Protocol().Code != ma.P_P2P {
		return "", wire.ErrFormat
	}
	return wire.ParsePeerID(last.Value())
}

// HintsFromMember parses the already-authenticated membership record's rkey and
// addrs fields. Record decoding/signature verification belongs to cluster state.
func HintsFromMember(peer string, addresses []string, l HintLimits) (Hint, error) {
	if len(peer) > 128 || len(addresses) > l.MaxAddresses || l.MaxAddresses < 0 {
		return Hint{}, wire.ErrLimit
	}
	id, err := wire.ParsePeerID(peer)
	if err != nil {
		return Hint{}, err
	}
	h := Hint{Peer: id}
	total := len(peer)
	for _, text := range addresses {
		total += len(text)
		if total > l.MaxBytes {
			return Hint{}, wire.ErrLimit
		}
		a, err := wire.ParseMultiaddr(text, l.MaxAddressBytes)
		if err != nil {
			return Hint{}, err
		}
		a, err = NormalizeAddress(id, a)
		if err != nil {
			return Hint{}, err
		}
		h.Addresses = append(h.Addresses, a)
	}
	return h, nil
}

// ParseHints extracts bounded peer/addr query hints without resolving the CID,
// name, or via DID. With multiple peers, each address must carry its destination
// /p2p suffix; suffix-less addresses require exactly one peer. Unknown link query
// keys are ignored for forward compatibility. Hints confer no authorization.
func ParseHints(link string, l HintLimits) ([]Hint, error) {
	if l.MaxBytes < 0 || len(link) > l.MaxBytes {
		return nil, wire.ErrLimit
	}
	if !strings.HasPrefix(link, "nu://") || strings.ContainsAny(link, "\r\n\x00#") {
		return nil, wire.ErrFormat
	}
	target, query, _ := strings.Cut(strings.TrimPrefix(link, "nu://"), "?")
	if target == "" {
		return nil, wire.ErrFormat
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return nil, err
	}
	if len(q["peer"]) > l.MaxPeers || len(q["addr"]) > l.MaxAddresses || l.MaxPeers < 0 || l.MaxAddresses < 0 {
		return nil, wire.ErrLimit
	}
	var out []Hint
	indexes := make(map[wire.PeerID]int)
	addPeer := func(id wire.PeerID) error {
		if _, ok := indexes[id]; ok {
			return nil
		}
		if len(out) >= l.MaxPeers {
			return wire.ErrLimit
		}
		indexes[id] = len(out)
		out = append(out, Hint{Peer: id})
		return nil
	}
	for _, text := range q["peer"] {
		id, err := wire.ParsePeerID(text)
		if err != nil {
			return nil, err
		}
		if err := addPeer(id); err != nil {
			return nil, err
		}
	}
	explicitPeers := len(out)
	for _, text := range q["addr"] {
		a, err := wire.ParseMultiaddr(text, l.MaxAddressBytes)
		if err != nil {
			return nil, err
		}
		id, peerErr := AddressPeer(a)
		if peerErr != nil {
			if len(out) != 1 {
				return nil, wire.ErrFormat
			}
			id = out[0].Peer
		}
		if _, ok := indexes[id]; !ok && explicitPeers > 0 {
			return nil, ErrPeerMismatch
		}
		if err := addPeer(id); err != nil {
			return nil, err
		}
		a, err = NormalizeAddress(id, a)
		if err != nil {
			return nil, err
		}
		i := indexes[id]
		out[i].Addresses = append(out[i].Addresses, a)
	}
	return out, nil
}
