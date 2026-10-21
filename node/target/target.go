// Package target parses the argument of get/cat/ls per spec.md "Targets": links, CIDs
// with paths, public names and datasets. Parsing is syntactic; whether a kind is supported
// by this build is a separate check (Target.Supported).
package target

import (
	"regexp"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
)

// Kind is the form a target took.
type Kind string

const (
	// KindCID is immutable content: a `b…` CID with an optional `/path`.
	KindCID Kind = "cid"
	// KindLink is a `nu://` share link: CID and Path are set from it and Link holds the
	// provider hints.
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
	// CID is set for KindCID and KindLink.
	CID string
	// Path is the optional sub-path, without a leading slash, for CID, link, name and
	// dataset targets.
	Path string
	// Link is the parsed share link of a KindLink target.
	Link *link.Link
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

// cidTextLen is the length of a CID's text form; a string of that length starting with
// 'b' that does not parse is reported as a bad CID rather than a dataset name.
const cidTextLen = 1 + (cid.Size*8+4)/5

var (
	datasetRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Parse parses s. Errors are *apiv0.Error with code invalid_argument and list the forms.
func Parse(s string) (Target, error) {
	t := Target{Raw: s}
	if s == "" {
		return t, invalid("missing target")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		switch strings.ToLower(s[:i]) {
		case link.Scheme:
			l, err := link.Parse(s)
			if err != nil {
				return t, invalid("%v", err)
			}
			t.Kind, t.Link, t.CID, t.Path = KindLink, l, l.CID.String(), l.Path
			return t, nil
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
	if len(first) == cidTextLen && first[0] == 'b' {
		return t, invalid("%q is not a DASL CID (raw or DRISL, SHA-256 or BLAKE3)", first)
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

// IsCID reports whether s is a CID this build can act on: the canonical text form of a
// DASL/BDASL CIDv1 (raw or DRISL, SHA-256 or BLAKE3), per core/cid.
func IsCID(s string) bool {
	_, err := cid.Parse(s)
	return err == nil
}

// Supported returns nil when this build can act on the target, else a not_supported error
// saying what to do. CID targets, CID share links and at:// targets are supported;
// rasl://, names and datasets are not yet.
func (t Target) Supported() error {
	switch t.Kind {
	case KindCID, KindLink, KindAtproto:
		return nil
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
