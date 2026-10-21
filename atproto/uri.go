package atproto

import (
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/whyrusleeping/nu/api/apiv0"
)

// URI is a parsed at:// target: at://<handle-or-did>[@<rev>][/<collection>[/<rkey>]].
// The @<rev> suffix is a Nu extension selecting one held snapshot (the gateway's
// /at/<did>@<rev>/… form); it is never sent to a PDS.
type URI struct {
	// Authority is the handle or DID as given (handles are lower-cased).
	Authority string
	// Rev selects a held snapshot by commit rev; "" is the latest.
	Rev        string
	Collection string
	RKey       string
}

// IsDID reports whether the authority is a DID (else a handle).
func (u URI) IsDID() bool { return strings.HasPrefix(u.Authority, "did:") }

// IsRecord reports whether the URI names one record.
func (u URI) IsRecord() bool { return u.RKey != "" }

// Path is "<collection>/<rkey>" for a record URI.
func (u URI) Path() string { return u.Collection + "/" + u.RKey }

// String renders the URI with the authority given.
func (u URI) String() string {
	s := "at://" + u.Authority
	if u.Rev != "" {
		s += "@" + u.Rev
	}
	if u.Collection != "" {
		s += "/" + u.Collection
		if u.RKey != "" {
			s += "/" + u.RKey
		}
	}
	return s
}

// ParseURI parses an at:// target. Errors are invalid_argument.
func ParseURI(s string) (URI, error) {
	var u URI
	rest, ok := strings.CutPrefix(strings.ToLower(s[:min(len(s), 5)])+s[min(len(s), 5):], "at://")
	if !ok {
		return u, invalid("%q is not an at:// URI", s)
	}
	if strings.ContainsAny(rest, "?#") {
		return u, invalid("at:// targets take no query or fragment: %q", s)
	}
	auth, path, _ := strings.Cut(rest, "/")
	if auth == "" {
		return u, invalid("%q has no handle or DID", s)
	}
	auth, rev, hasRev := strings.Cut(auth, "@")
	if hasRev {
		if _, err := syntax.ParseTID(rev); err != nil {
			return u, invalid("%q: @%s is not a commit rev (TID)", s, rev)
		}
		u.Rev = rev
	}
	if strings.HasPrefix(auth, "did:") {
		d, err := syntax.ParseDID(auth)
		if err != nil {
			return u, invalid("%q: %v", s, err)
		}
		u.Authority = d.String()
	} else {
		h, err := syntax.ParseHandle(auth)
		if err != nil {
			return u, invalid("%q: %v", s, err)
		}
		u.Authority = h.Normalize().String()
	}
	if path == "" {
		return u, nil
	}
	coll, rkey, hasKey := strings.Cut(path, "/")
	n, err := syntax.ParseNSID(coll)
	if err != nil {
		return u, invalid("%q: collection %q: %v", s, coll, err)
	}
	u.Collection = n.String()
	if hasKey {
		if strings.Contains(rkey, "/") || rkey == "" {
			return u, invalid("%q: an at:// target is at most <authority>/<collection>/<rkey>", s)
		}
		k, err := syntax.ParseRecordKey(rkey)
		if err != nil {
			return u, invalid("%q: record key %q: %v", s, rkey, err)
		}
		u.RKey = k.String()
	}
	return u, nil
}

func invalid(format string, args ...any) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, format, args...).
		WithAction("give at://<handle-or-did>[@<rev>][/<collection>[/<rkey>]]")
}
