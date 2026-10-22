// Package fisl implements the client side of DASL FISL (https://dasl.ing/fisl.html, D21
// "FISL", D15): asking configured indexers `GET /routing/locations/<cid>/jsonl|json`
// for the locations of a CID, validating the answer under fixed bounds, and fetching the
// usable https locations through rasl's verified retrieval (verify-before-store,
// origin and publisher demotion on mismatch, httpsrc's SSRF policy). Serving FISL and a
// Nu endpoint URL scheme are later work.
package fisl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	// PathPrefix is the lookup path on an indexer; the CID and format follow.
	PathPrefix = "/routing/locations/"
	// MaxResponseBytes bounds one indexer response body.
	MaxResponseBytes = 1 << 20
	// MaxLineBytes bounds one JSONL record (and one JSON record).
	MaxLineBytes = 64 << 10
	// MaxRecords bounds the records kept from one response; later ones are ignored.
	MaxRecords = 64
	// MaxAddrs bounds the addrs kept from one record; later ones are ignored.
	MaxAddrs = 16
	// MaxURLLen bounds a location URL (and a configured indexer URL).
	MaxURLLen = 2048
	// MaxPublisherLen bounds a publisher DID.
	MaxPublisherLen = 2048
)

// Addr is one location of a record: an absolute URL and its effective expiry (the
// earliest of its own and the record's; zero when neither is stated).
type Addr struct {
	URL     string
	Expires time.Time
}

// Location is one FISL record: who announced the addrs (a DID, or "" when the indexer
// does not know), the addrs, the unverified size hint (-1 when absent) and the record's
// expiry (zero when absent).
type Location struct {
	Publisher string
	Addrs     []Addr
	Size      int64
	Expires   time.Time
}

// ErrFormat is a response that is not a FISL body.
var ErrFormat = errors.New("fisl: malformed response")

// record is the wire shape; unknown fields are ignored, as the spec asks.
type record struct {
	Publisher json.RawMessage `json:"publisher"`
	Addrs     []struct {
		URL     json.RawMessage `json:"url"`
		Expires json.RawMessage `json:"expires"`
	} `json:"addrs"`
	Size    json.RawMessage `json:"size"`
	Expires json.RawMessage `json:"expires"`
}

// DecodeJSONL parses a jsonl body (one record per line) incrementally, keeping at most
// MaxRecords valid, unexpired records with their unexpired addrs. A malformed line ends
// parsing: the records before it are returned with ErrFormat. The reader must already
// be bounded (MaxResponseBytes).
func DecodeJSONL(r io.Reader, now time.Time) ([]Location, error) {
	br := bufio.NewReaderSize(r, 16<<10)
	var out []Location
	for len(out) < MaxRecords {
		line, err := readLine(br)
		if len(bytes.TrimSpace(line)) > 0 {
			var rec record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return out, fmt.Errorf("%w: %v", ErrFormat, jerr)
			}
			if loc, ok := rec.location(now); ok {
				out = append(out, loc)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// readLine reads one line of at most MaxLineBytes (ErrFormat beyond that).
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxLineBytes {
			return nil, fmt.Errorf("%w: line longer than %d bytes", ErrFormat, MaxLineBytes)
		}
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// DecodeJSON parses a json body (`{"locations": [...]}`) record by record, under the
// same bounds as DecodeJSONL. Other top-level members are ignored.
func DecodeJSON(r io.Reader, now time.Time) ([]Location, error) {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%w: not a JSON object", ErrFormat)
	}
	var out []Location
	seen := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return out, fmt.Errorf("%w: %v", ErrFormat, err)
		}
		key, _ := tok.(string)
		if key != "locations" || seen {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return out, fmt.Errorf("%w: %v", ErrFormat, err)
			}
			continue
		}
		seen = true
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return out, fmt.Errorf("%w: locations is not an array", ErrFormat)
		}
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return out, fmt.Errorf("%w: %v", ErrFormat, err)
			}
			if len(out) >= MaxRecords {
				continue
			}
			if len(raw) > MaxLineBytes {
				return out, fmt.Errorf("%w: record longer than %d bytes", ErrFormat, MaxLineBytes)
			}
			var rec record
			if err := json.Unmarshal(raw, &rec); err != nil {
				return out, fmt.Errorf("%w: %v", ErrFormat, err)
			}
			if loc, ok := rec.location(now); ok {
				out = append(out, loc)
			}
		}
		if _, err := dec.Token(); err != nil { // ]
			return out, fmt.Errorf("%w: %v", ErrFormat, err)
		}
	}
	if _, err := dec.Token(); err != nil { // }
		return out, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if !seen {
		return nil, fmt.Errorf("%w: no locations member", ErrFormat)
	}
	return out, nil
}

// location validates one record: the addrs array is required and non-empty; an
// unparseable or past expiry drops the record (or the addr); addrs without a parseable
// absolute URL are dropped; a publisher that is not a DID string is treated as absent
// (the method is never judged); a size that is not a non-negative integer is -1.
func (rec *record) location(now time.Time) (Location, bool) {
	loc := Location{Size: -1}
	var ok bool
	if loc.Expires, ok = expiry(rec.Expires); !ok || (!loc.Expires.IsZero() && !loc.Expires.After(now)) {
		return Location{}, false
	}
	if s := jsonString(rec.Publisher); strings.HasPrefix(s, "did:") && len(s) <= MaxPublisherLen && !strings.ContainsAny(s, " \t\r\n") {
		loc.Publisher = s
	}
	var size int64
	if len(rec.Size) > 0 && json.Unmarshal(rec.Size, &size) == nil && size >= 0 {
		loc.Size = size
	}
	for _, a := range rec.Addrs {
		if len(loc.Addrs) == MaxAddrs {
			break
		}
		exp, ok := expiry(a.Expires)
		if !ok {
			continue
		}
		if exp.IsZero() || (!loc.Expires.IsZero() && loc.Expires.Before(exp)) {
			exp = loc.Expires
		}
		if !exp.IsZero() && !exp.After(now) {
			continue
		}
		raw := jsonString(a.URL)
		if raw == "" || len(raw) > MaxURLLen {
			continue
		}
		if u, err := url.Parse(raw); err != nil || u.Scheme == "" || !u.IsAbs() {
			continue
		}
		loc.Addrs = append(loc.Addrs, Addr{URL: raw, Expires: exp})
	}
	return loc, len(loc.Addrs) > 0
}

func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// expiry parses an optional datetime (RFC 3339 with a zone, as the atproto Lexicon
// datetime). Absent is (zero, true); unparseable is (zero, false).
func expiry(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return time.Time{}, true
	}
	s := jsonString(raw)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ParseIndexer validates a configured indexer base URL: absolute http(s), a host, an
// optional path prefix, no query, fragment or userinfo. The result has no trailing slash.
func ParseIndexer(s string) (string, error) {
	if len(s) > MaxURLLen {
		return "", fmt.Errorf("fisl: indexer URL longer than %d bytes", MaxURLLen)
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("fisl: indexer URL %q: %v", s, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("fisl: indexer URL %q must be https://host[:port][/prefix]", s)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// LookupURL is the lookup URL of c (in string form) on an indexer base, in format
// ("jsonl" or "json").
func LookupURL(indexer, cidStr, format string) string {
	return indexer + PathPrefix + cidStr + "/" + format
}
