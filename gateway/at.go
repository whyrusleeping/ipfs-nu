package gateway

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
)

// /at/<did>[@<rev>][/<collection>[/<rkey>]] serves held atproto snapshots (contract G5,
// N30; D21 "Using atproto data"): collection and record listings are gateway pages with
// the listing CSP, a record is application/json, and blob references are left as CIDs
// for the /blob/ download route. Nothing here is ever active content, there is no site
// origin for /at/, and a live repository is never fetched on request (P4).

var revRE = regexp.MustCompile(`^[234567a-z]{13}$`)

// atTarget parses the {did} path segment: a DID with an optional @rev.
func atTarget(seg string) (did, rev string, err error) {
	did, rev, hasRev := strings.Cut(seg, "@")
	if _, perr := syntax.ParseDID(did); perr != nil {
		return "", "", apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a DID", did).WithAction("use /at/<did>[@<rev>]/<collection>/<rkey>")
	}
	if hasRev && !revRE.MatchString(rev) {
		return "", "", apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a commit rev", rev)
	}
	return did, rev, nil
}

func (h *Handler) atContent() (AtContent, error) {
	ac, ok := h.content.(AtContent)
	if !ok || h.content == nil {
		return nil, apiv0.NotImplemented("atproto snapshots on the gateway")
	}
	return ac, nil
}

// atHeaders sets the shared headers: snapshots at a fixed rev are immutable, the latest
// view re-validates like a name route (G9).
func atHeaders(w http.ResponseWriter, rev string) {
	if rev != "" {
		w.Header().Set("Cache-Control", immutableCache)
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
}

type atListingData struct {
	Name, DID, Rev, Commit, Collection string
	Collections                        []atRow
	Records                            []atRow
}

// atRow is a listing row; Href is empty when the name is not linkable (not a valid NSID
// or record key: shown, not linked, like unsafe tree entries).
type atRow struct {
	Name, Href, CID string
	Size            int64
}

var atPage = template.Must(template.New("at").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Name}}: at://{{.DID}}{{if .Collection}}/{{.Collection}}{{end}}</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:60em;margin:2em auto;padding:0 1em}table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.2em .6em;border-bottom:1px solid #eee}td.n{text-align:right}code{font-size:.9em;word-break:break-all}</style>
<h1>at://{{.DID}}{{if .Collection}}/{{.Collection}}{{end}}</h1>
<p>snapshot at rev <code>{{.Rev}}</code>, commit <code>{{.Commit}}</code>{{if not .Collection}} · <a href="/at/{{.DID}}@{{.Rev}}/">this revision</a>{{end}}</p>
{{if .Collection}}<table><tr><th>record key</th><th class="n">size</th><th>cid</th></tr>
<tr><td><a href="../">../</a></td><td></td><td></td></tr>
{{range .Records}}<tr><td>{{if .Href}}<a href="{{.Href}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td><td class="n">{{.Size}}</td><td><code>{{.CID}}</code></td></tr>
{{end}}</table>
{{else}}<table><tr><th>collection</th><th class="n">records</th></tr>
{{range .Collections}}<tr><td>{{if .Href}}<a href="{{.Href}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td><td class="n">{{.Size}}</td></tr>
{{end}}</table>{{end}}
`))

func (h *Handler) at(w http.ResponseWriter, r *http.Request) {
	ac, err := h.atContent()
	if err != nil {
		writeError(w, err)
		return
	}
	if strings.Contains(r.URL.Path, "//") {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid path %q", r.URL.Path))
		return
	}
	did, rev, err := atTarget(r.PathValue("did"))
	if err != nil {
		writeError(w, err)
		return
	}
	rest := strings.Trim(r.PathValue("path"), "/")
	if err := validatePath(rest); err != nil {
		writeError(w, err)
		return
	}
	var collection, rkey string
	if rest != "" {
		var more bool
		collection, rkey, more = strings.Cut(rest, "/")
		if n, err := syntax.ParseNSID(collection); err != nil {
			writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a collection NSID", collection))
			return
		} else {
			collection = n.String()
		}
		if more {
			if strings.Contains(rkey, "/") {
				writeError(w, apiv0.Errorf(apiv0.CodeNotFound, "records have no sub-paths").WithAction("use /at/<did>/<collection>/<rkey>"))
				return
			}
			if _, err := syntax.ParseRecordKey(rkey); err != nil {
				writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a record key", rkey))
				return
			}
		}
	}
	snap, err := ac.AtSnapshot(r.Context(), did, rev)
	if err != nil {
		writeError(w, err)
		return
	}
	atHeaders(w, rev)
	if rkey != "" {
		js, c, err := ac.AtRecordJSON(r.Context(), snap, collection, rkey)
		if err != nil {
			writeError(w, err)
			return
		}
		hdr := w.Header()
		// Inert rendering (N30): JSON, never HTML; the CSP forbids everything even if a
		// browser were to treat it as a document.
		hdr.Set("Content-Type", "application/json; charset=utf-8")
		hdr.Set("Content-Security-Policy", listingCSP)
		hdr.Set("ETag", `"`+c+`"`)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(js)
		return
	}
	// Listings redirect to the trailing-slash form so relative links work (G6).
	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.EscapedPath()+"/", http.StatusMovedPermanently)
		return
	}
	data := atListingData{Name: nu.Name, DID: snap.DID, Rev: snap.Rev, Commit: snap.Commit, Collection: collection}
	if collection != "" {
		recs, err := ac.AtRecords(r.Context(), snap, collection)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, rec := range recs {
			row := atRow{Name: rec.RKey, CID: rec.CID, Size: rec.Size}
			if _, err := syntax.ParseRecordKey(rec.RKey); err == nil && rec.RKey != "." && rec.RKey != ".." {
				row.Href = url.PathEscape(rec.RKey)
			}
			data.Records = append(data.Records, row)
		}
	} else {
		cols, err := ac.AtCollections(r.Context(), snap)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, c := range cols {
			row := atRow{Name: c.Name, Size: int64(c.Count)}
			if _, err := syntax.ParseNSID(c.Name); err == nil {
				row.Href = url.PathEscape(c.Name) + "/"
			}
			data.Collections = append(data.Collections, row)
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", listingCSP)
	if r.Method == http.MethodHead {
		return
	}
	_ = atPage.Execute(w, data)
}
