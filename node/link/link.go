// Package link formats and parses Nu share links (docs/names.md "Links"):
//
//	nu://<cid>[/<path>][?peer=<base36 peerid>[&addr=<multiaddr>]…[&via=<did>]]
//
// The CID comes first so a link is readable in chat; hints are few and optional. A
// `peer=` names a node that holds the content; the `addr=` values that follow it (until
// the next `peer=`) are that node's transport multiaddrs, for recipients that cannot look
// the peer up (no dnsaddr, routing or mDNS yet). `via=` names an atproto DID that can be
// asked for the peer's current addresses; this build carries it but does not act on it.
//
// Parsing is strict and bounded: canonical DASL CID, a clean relative path, known query
// keys only, at most MaxProviders peers and MaxAddrs addresses per peer, and MaxLen bytes
// in total. The parser never dials or resolves anything.
package link

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Scheme is the URL scheme of a share link.
const Scheme = "nu"

// Limits of a link this package accepts or produces.
const (
	// MaxLen bounds the whole link text.
	MaxLen = 2048
	// MaxProviders bounds the `peer=` hints.
	MaxProviders = 8
	// MaxAddrs bounds the `addr=` hints per peer.
	MaxAddrs = 8
	// MaxAddrLen bounds one multiaddr's text.
	MaxAddrLen = 256
	// MaxPathLen bounds the path after the CID.
	MaxPathLen = 1024
	// MaxViaLen bounds a `via=` DID.
	MaxViaLen = 256
)

// Link is a parsed or to-be-formatted share link.
type Link struct {
	// CID is the shared root.
	CID cid.CID
	// Path is the optional slash-separated path under the root, without a leading slash.
	Path string
	// Providers are the peers hinted as holders, in link order.
	Providers []Provider
	// Via is an atproto DID that can supply the providers' addresses (reserved).
	Via string
}

// Provider is one `peer=` hint with its `addr=` values.
type Provider struct {
	Peer  wire.PeerID
	Addrs []wire.Multiaddr
}

// Error is a parse failure; the message says what is wrong in plain language.
type Error struct{ Msg string }

func (e *Error) Error() string { return "link: " + e.Msg }

func bad(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// IsLink reports whether s starts with the nu:// scheme (case-insensitive). It does not
// validate the rest.
func IsLink(s string) bool {
	return len(s) >= len(Scheme)+3 && strings.EqualFold(s[:len(Scheme)+3], Scheme+"://")
}

// Parse parses a share link. Errors are *Error.
func Parse(s string) (*Link, error) {
	if len(s) > MaxLen {
		return nil, bad("link is longer than %d bytes", MaxLen)
	}
	if !IsLink(s) {
		return nil, bad("a share link starts with %s://", Scheme)
	}
	rest := s[len(Scheme)+3:]
	if strings.ContainsAny(rest, " \t\r\n\x00#") {
		return nil, bad("link contains whitespace, NUL or a fragment")
	}
	rest, query, hasQuery := strings.Cut(rest, "?")
	cidText, path, hasPath := strings.Cut(rest, "/")
	if cidText == "" {
		return nil, bad("missing CID after %s://", Scheme)
	}
	if hasPath && path == "" {
		return nil, bad("path has an empty segment or trailing slash")
	}
	c, err := cid.Parse(cidText)
	if err != nil {
		return nil, bad("%q is not a DASL CID (raw or DRISL, SHA-256 or BLAKE3, lowercase base32)", cidText)
	}
	l := &Link{CID: c}
	if path != "" {
		if l.Path, err = parsePath(path); err != nil {
			return nil, err
		}
	}
	if hasQuery {
		if err := l.parseQuery(query); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// parsePath decodes and checks the path: percent-decoded, no empty, ".", ".." or NUL
// segments, no trailing slash.
func parsePath(p string) (string, error) {
	if len(p) > MaxPathLen {
		return "", bad("path is longer than %d bytes", MaxPathLen)
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", bad("path segment %q is not valid percent-encoding", seg)
		}
		switch {
		case dec == "":
			return "", bad("path has an empty segment or trailing slash")
		case dec == "." || dec == "..":
			return "", bad("path segment %q is not allowed", dec)
		case strings.ContainsAny(dec, "/\x00"):
			return "", bad("path segment %q contains a slash or NUL", seg)
		}
		segs[i] = dec
	}
	return strings.Join(segs, "/"), nil
}

func (l *Link) parseQuery(q string) error {
	if q == "" {
		return bad("empty query")
	}
	var cur *Provider
	for _, kv := range strings.Split(q, "&") {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return bad("query parameter %q is not key=value", kv)
		}
		val, err := url.QueryUnescape(val)
		if err != nil {
			return bad("query parameter %q is not valid percent-encoding", kv)
		}
		switch key {
		case "peer":
			if len(l.Providers) == MaxProviders {
				return bad("more than %d peer hints", MaxProviders)
			}
			id, err := wire.ParsePeerID(val)
			if err != nil {
				return bad("%q is not a peer ID", val)
			}
			for _, p := range l.Providers {
				if p.Peer == id {
					return bad("peer %s is listed twice", id.Base36())
				}
			}
			l.Providers = append(l.Providers, Provider{Peer: id})
			cur = &l.Providers[len(l.Providers)-1]
		case "addr":
			if cur == nil {
				return bad("addr=%s comes before any peer=", val)
			}
			if len(cur.Addrs) == MaxAddrs {
				return bad("more than %d addresses for peer %s", MaxAddrs, cur.Peer.Base36())
			}
			a, err := parseAddr(val, cur.Peer)
			if err != nil {
				return err
			}
			cur.Addrs = append(cur.Addrs, a)
		case "via":
			if l.Via != "" {
				return bad("via= is given twice")
			}
			if len(val) > MaxViaLen || !strings.HasPrefix(val, "did:") || strings.Count(val, ":") < 2 {
				return bad("via=%q is not a DID", val)
			}
			l.Via = val
		default:
			return bad("unknown query parameter %q", key)
		}
	}
	return nil
}

// parseAddr parses one transport hint. Direct hints omit /p2p (peer= binds
// them). A single literal circuit hop must carry its relay identity and its
// terminal destination must match peer=. Parsing grants no dial authorization.
func parseAddr(s string, peer wire.PeerID) (wire.Multiaddr, error) {
	if len(s) > MaxAddrLen {
		return wire.Multiaddr{}, bad("address is longer than %d bytes", MaxAddrLen)
	}
	a, err := wire.ParseMultiaddr(s, MaxAddrLen)
	if err != nil {
		if errors.Is(err, wire.ErrLimit) {
			return wire.Multiaddr{}, bad("address %q is too long", s)
		}
		return wire.Multiaddr{}, bad("%q is not a multiaddr", s)
	}
	if a.Opaque() {
		return wire.Multiaddr{}, bad("address %q uses an unknown protocol", s)
	}
	if strings.Contains(a.String(), "/p2p-circuit") {
		_, _, destination, err := circuit.ParseAddress(a)
		if err != nil || destination != peer {
			return wire.Multiaddr{}, bad("circuit address must have one supported relay hop and match peer=")
		}
		return a, nil
	}
	if strings.Contains(a.String(), "/p2p/") {
		return wire.Multiaddr{}, bad("address %q names a peer; give the peer with peer= and the transport address with addr=", s)
	}
	return a, nil
}

// String formats the link. Providers beyond MaxProviders and addresses beyond MaxAddrs
// are dropped, so the output always parses (given a defined CID and clean path).
func (l *Link) String() string {
	var b strings.Builder
	b.WriteString(Scheme + "://")
	b.WriteString(l.CID.String())
	if l.Path != "" {
		for _, seg := range strings.Split(l.Path, "/") {
			b.WriteByte('/')
			b.WriteString(escapeValue(seg))
		}
	}
	sep := byte('?')
	param := func(k, v string) {
		b.WriteByte(sep)
		sep = '&'
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(escapeValue(v))
	}
	for i, p := range l.Providers {
		if i == MaxProviders {
			break
		}
		param("peer", p.Peer.Base36())
		for j, a := range p.Addrs {
			if j == MaxAddrs {
				break
			}
			if a.Opaque() {
				continue
			}
			param("addr", a.String())
		}
	}
	if l.Via != "" {
		param("via", l.Via)
	}
	return b.String()
}

// Target returns the CID-with-path form of the link (`<cid>[/path]`), which the content
// commands resolve.
func (l *Link) Target() string {
	if l.Path == "" {
		return l.CID.String()
	}
	return l.CID.String() + "/" + l.Path
}

// escapeValue percent-encodes a query value, keeping the characters multiaddrs and DIDs
// are made of (letters, digits, "-._~:/") readable; everything else, including "&", "=",
// "+" and "%", is encoded so url.QueryUnescape inverts it exactly.
func escapeValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == ':', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
