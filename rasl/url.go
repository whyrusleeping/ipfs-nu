// Package rasl implements DASL RASL (https://dasl.ing/rasl.html, D21): parsing and
// formatting `rasl://<cid>/?hint=<host>…` URLs, and fetching a CID over HTTPS from its
// hinted hosts (`https://<host>/.well-known/rasl/<cid>`) through httpsrc's SSRF policy,
// verified by CID before anything is stored. The gateway side (serving
// `/.well-known/rasl/<cid>` for held content) lives in package gateway.
package rasl

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
)

const (
	// Scheme is the URL scheme.
	Scheme = "rasl"
	// WellKnown is the retrieval path prefix on a hinted host.
	WellKnown = "/.well-known/rasl/"
	// MaxURLLen bounds a rasl:// URL's text.
	MaxURLLen = 2048
	// MaxHints bounds the hints kept from one URL (later ones are dropped).
	MaxHints = 16
)

// URL is a parsed rasl:// URL: the CID is the authority, the hints are hosts that may
// serve it. The path is ignored, as the spec asks.
type URL struct {
	CID   cid.CID
	Hints []string
}

// LegacyScheme is the pre-spec form `web+rasl://<cid>;<host>,<host>/` still emitted by
// @dasl/rasl 1.x; it is accepted on input and never produced.
const LegacyScheme = "web+rasl"

// IsURL reports whether s starts with rasl:// (or the legacy web+rasl://).
func IsURL(s string) bool {
	return hasScheme(s, Scheme) || hasScheme(s, LegacyScheme)
}

func hasScheme(s, scheme string) bool {
	return len(s) >= len(scheme)+3 && strings.EqualFold(s[:len(scheme)+3], scheme+"://")
}

// Parse parses s per the spec's "parse a RASL URL": the host must be a DASL CID; every
// `hint` query value that is a valid https host (optionally with a port) is kept, in
// order, without duplicates; other values are ignored. The path is ignored.
func Parse(s string) (URL, error) {
	if len(s) > MaxURLLen {
		return URL{}, &Error{"URL is longer than " + strconv.Itoa(MaxURLLen) + " bytes"}
	}
	if !IsURL(s) {
		return URL{}, &Error{"a RASL URL starts with rasl://"}
	}
	var host string
	var hints []string
	if hasScheme(s, LegacyScheme) {
		rest, _, _ := strings.Cut(s[len(LegacyScheme)+3:], "/")
		rest, _, _ = strings.Cut(rest, "?")
		var list string
		host, list, _ = strings.Cut(rest, ";")
		for _, h := range strings.Split(list, ",") {
			if h, err := url.PathUnescape(h); err == nil {
				hints = append(hints, h)
			}
		}
	} else {
		u, err := url.Parse(s)
		if err != nil {
			return URL{}, &Error{err.Error()}
		}
		host, hints = u.Host, u.Query()["hint"]
	}
	c, err := cid.Parse(host)
	if err != nil {
		return URL{}, &Error{strconv.Quote(host) + " is not a DASL CID (raw or DRISL, SHA-256 or BLAKE3, lowercase base32)"}
	}
	r := URL{CID: c}
	seen := map[string]bool{}
	for _, h := range hints {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if !ValidHint(h) || seen[h] {
			continue
		}
		if len(r.Hints) == MaxHints {
			break
		}
		seen[h] = true
		r.Hints = append(r.Hints, h)
	}
	return r, nil
}

// String formats the URL as rasl://<cid>/?hint=…&hint=….
func (u URL) String() string {
	s := Scheme + "://" + u.CID.String() + "/"
	for i, h := range u.Hints {
		if i == 0 {
			s += "?"
		} else {
			s += "&"
		}
		s += "hint=" + url.QueryEscape(h)
	}
	return s
}

// HintURL is the retrieval URL of c on hint: <scheme>://<hint>/.well-known/rasl/<cid>.
func HintURL(scheme, hint string, c cid.CID) string {
	return scheme + "://" + hint + WellKnown + c.String()
}

// ValidHint reports whether h is a valid https host: a DNS name or IP literal (IPv6 in
// brackets), optionally followed by :port. Hosts are compared lowercase.
func ValidHint(h string) bool {
	if h == "" || len(h) > 260 {
		return false
	}
	host, port := h, ""
	if strings.HasPrefix(h, "[") {
		end := strings.IndexByte(h, ']')
		if end < 0 {
			return false
		}
		host, port = h[1:end], h[end+1:]
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() || a.Zone() != "" {
			return false
		}
		return port == "" || (port[0] == ':' && validPort(port[1:]))
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		host, port = h[:i], h[i+1:]
		if !validPort(port) {
			return false
		}
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Is4()
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && p == strconv.Itoa(n) && n > 0 && n < 65536
}

// Error is a URL parse error.
type Error struct{ Msg string }

func (e *Error) Error() string { return "rasl: " + e.Msg }
