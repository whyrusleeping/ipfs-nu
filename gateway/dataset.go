package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/target"
)

// DatasetContent resolves a dataset of this node (its head, or a version selected by TID,
// label or time) to a root CID; content that implements it serves /c/ (0047).
type DatasetContent interface {
	ResolveDataset(ctx context.Context, name, version string) (string, error)
}

// AtDatasetContent is the optional atproto-archive view of a DatasetContent (0054): a
// dataset root that is a repository commit redirects to its /at/<did>@<rev>/ listing
// instead of /tree/.
type AtDatasetContent interface {
	AtLocation(ctx context.Context, root string) (string, bool)
}

// NameContent resolves a public name <handle-or-did>/<name> to its current verified
// target (cached per docs/names.md D6); content that implements it serves /nu/ (0045).
// held selects the follow's held root instead of the resolved target.
type NameContent interface {
	ResolveName(ctx context.Context, authority, name string, held bool) (root string, source string, err error)
}

// name serves /nu/<handle-or-did>/<name>/<path> as a redirect to the immutable
// /tree/<cid>/<path> URL of the resolved target (X-Nu-Root, X-Nu-Source: pds|cache|held).
// ?held=1 serves the local follow's held snapshot, which may be older than the name.
func (h *Handler) name(w http.ResponseWriter, r *http.Request) {
	if h.pub != nil {
		// P2/P4: names resolve live and would redirect to roots nobody published; the
		// public listener has no /nu/ route (0077).
		h.notFound(w, r)
		return
	}
	auth, name, p := r.PathValue("authority"), r.PathValue("name"), r.PathValue("path")
	if p != "" {
		if err := validatePath(p); err != nil {
			writeError(w, err)
			return
		}
	}
	nc, ok := h.content.(NameContent)
	if !ok || h.content == nil {
		writeError(w, apiv0.NotImplemented("names on the gateway"))
		return
	}
	root, source, err := nc.ResolveName(r.Context(), auth, name, r.URL.Query().Get("held") == "1")
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("X-Nu-Source", source)
	redirectRoot(w, r, root, p)
}

func redirectRoot(w http.ResponseWriter, r *http.Request, root, p string) {
	loc := "/tree/" + root + "/"
	if p != "" {
		var segs []string
		for _, s := range strings.Split(p, "/") {
			segs = append(segs, url.PathEscape(s))
		}
		loc += strings.Join(segs, "/")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Nu-Root", root)
	http.Redirect(w, r, loc, http.StatusFound)
}

// dataset serves /c/<dataset>[@<version>]/<path> as a redirect to the immutable
// /tree/<cid>/<path> URL of the resolved root, so a multi-file reader binds to one root
// and the bare origin's non-active rules apply. The redirect itself is never cached. A
// version that is not retained-complete is an error naming its availability, never a
// fallback to another version.
func (h *Handler) dataset(w http.ResponseWriter, r *http.Request) {
	if h.pub != nil {
		// P2: a dataset's roots (X-Nu-Root) are this node's, not published; the public
		// listener has no /c/ route (0077).
		h.notFound(w, r)
		return
	}
	seg, p := r.PathValue("ds"), r.PathValue("path")
	t, err := target.Parse(seg)
	if err != nil || t.Kind != target.KindDataset || t.Path != "" {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a dataset", seg).WithAction("use /c/<dataset>[@<version>]/<path>"))
		return
	}
	if p != "" {
		if err := validatePath(p); err != nil {
			writeError(w, err)
			return
		}
	}
	dc, ok := h.content.(DatasetContent)
	if !ok || h.content == nil {
		writeError(w, apiv0.NotImplemented("datasets on the gateway"))
		return
	}
	root, err := dc.ResolveDataset(r.Context(), t.Dataset, t.Version)
	if err != nil {
		writeError(w, err)
		return
	}
	if ac, ok := h.content.(AtDatasetContent); ok {
		if loc, ok := ac.AtLocation(r.Context(), root); ok {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Nu-Root", root)
			http.Redirect(w, r, loc+escapeSegments(p), http.StatusFound)
			return
		}
	}
	redirectRoot(w, r, root, p)
}

func escapeSegments(p string) string {
	if p == "" {
		return ""
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		segs = append(segs, url.PathEscape(s))
	}
	return strings.Join(segs, "/")
}
