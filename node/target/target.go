// Package target parses the argument of get/cat/ls per spec.md "Targets": links, CIDs
// with paths, public names and datasets. Parsing is syntactic; whether a kind is supported
// by this build is a separate check (Target.Supported).
package target

import (
	"encoding/base32"
	"regexp"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// Kind is the form a target took.
type Kind string

const (
	// KindCID is immutable content: a `b…` CID with an optional `/path`.
	KindCID Kind = "cid"
	// KindLink is a `nu://` share link.
	KindLink Kind = "link"
	// KindAtproto is an `at://` record or repo.
	KindAtproto Kind = "atproto"
	// KindRASL is a `rasl://` fetch.
	KindRASL Kind = "rasl"
	// KindName is a public name: `<handle-or-did>/<name>[/path]`.
	KindName Kind = "name"
	// KindDataset is a dataset of this daemon: `<dataset>[@<version>][/path]`.
	KindDataset Kind = "dataset"
)

// Target is a parsed argument.
type Target struct {
	Kind Kind
	// Raw is the argument as given.
	Raw string
	// CID is set for KindCID.
	CID string
	// Path is the optional sub-path, without a leading slash, for CID, name and dataset
	// targets.
	Path string
	// Authority is the handle or DID of a name target.
	Authority string
	// Name is the public name of a name target.
	Name string
	// Dataset and Version describe a dataset target; Version is empty for the current one.
	Dataset string
	Version string
}

// Forms is the plain-language list of accepted forms, used in errors.
const Forms = "a CID (b…[/path]), a link (nu://…, at://…, rasl://…), a public name (<handle-or-did>/<name>[/path]) or a dataset (<dataset>[@<version>][/path])"

var (
	cidEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	datasetRE   = regexp.MustCompile(`^[a-z0-9-]+$`)
	versionRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Parse parses s. Errors are *apiv0.Error with code invalid_argument and list the forms.
func Parse(s string) (Target, error) {
	t := Target{Raw: s}
	if s == "" {
		return t, invalid("missing target")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		switch strings.ToLower(s[:i]) {
		case "nu":
			t.Kind = KindLink
		case "at":
			t.Kind = KindAtproto
		case "rasl":
			t.Kind = KindRASL
		default:
			return t, invalid("unsupported scheme %q", s[:i])
		}
		if len(s) == i+3 {
			return t, invalid("empty %s link", s[:i])
		}
		return t, nil
	}
	first, rest, _ := strings.Cut(s, "/")
	if first == "" {
		return t, invalid("target starts with a slash")
	}
	if IsCID(first) {
		t.Kind, t.CID, t.Path = KindCID, first, rest
		return t, nil
	}
	if strings.Contains(first, ".") || strings.HasPrefix(first, "did:") {
		name, path, _ := strings.Cut(rest, "/")
		if name == "" {
			return t, invalid("a public name needs a name after the handle or DID: %s/<name>", first)
		}
		t.Kind, t.Authority, t.Name, t.Path = KindName, first, name, path
		return t, nil
	}
	ds, version, hasVersion := strings.Cut(first, "@")
	if datasetRE.MatchString(ds) && (!hasVersion || versionRE.MatchString(version)) {
		t.Kind, t.Dataset, t.Version, t.Path = KindDataset, ds, version, rest
		return t, nil
	}
	return t, invalid("%q is not a valid target", s)
}

// IsCID reports whether s looks like a base32 CIDv1 (`b` multibase prefix, lowercase
// base32 body that decodes and starts with version 1). Full validation of codec and hash
// belongs to the CID package.
func IsCID(s string) bool {
	if len(s) < 2 || s[0] != 'b' {
		return false
	}
	body := s[1:]
	b, err := cidEncoding.DecodeString(body)
	if err != nil || len(b) < 4 {
		return false
	}
	return b[0] == 0x01
}

// Supported returns nil when this build can act on the target, else a not_supported error
// saying what to do. Only CID targets are supported before networking, names and datasets
// ship.
func (t Target) Supported() error {
	switch t.Kind {
	case KindCID:
		return nil
	case KindLink:
		return &apiv0.Error{Code: apiv0.CodeNotSupported, Message: "nu:// links are not supported yet",
			Action: "use the CID from the link directly (the part after nu://) until share links ship"}
	case KindAtproto:
		return &apiv0.Error{Code: apiv0.CodeNotSupported, Message: "at:// targets are not supported yet",
			Action: "atproto record and repo fetch arrive after v0; use a CID for now"}
	case KindRASL:
		return &apiv0.Error{Code: apiv0.CodeNotSupported, Message: "rasl:// targets are not supported yet",
			Action: "RASL retrieval arrives after v0; use a CID for now"}
	case KindName:
		return &apiv0.Error{Code: apiv0.CodeNotSupported, Message: "public names (" + t.Authority + "/" + t.Name + ") are not supported yet",
			Action: "names arrive after v0; ask the publisher for the CID and use that"}
	case KindDataset:
		return &apiv0.Error{Code: apiv0.CodeNotSupported, Message: "datasets (" + t.Dataset + ") are not supported yet",
			Action: "datasets arrive after v0; use a CID for now"}
	}
	return invalid("%q is not a valid target", t.Raw)
}

func invalid(format string, args ...any) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, format, args...).
		WithAction("give " + Forms)
}
