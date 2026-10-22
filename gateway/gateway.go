// Package gateway serves verified content to browsers on the loopback gateway listener.
//
// Two origins exist. The bare loopback origin (http://localhost:8171, http://127.0.0.1:8171)
// serves `/tree/<cid>/…` and `/blob/<cid>` as non-active content only: directory
// listings, files of inline-safe types, and everything else as a download. One origin per
// root, http://<cid>.localhost:8171/…, serves a tree as a site (index.html, listings,
// active content allowed) because origin isolation, not the gateway, keeps it from
// touching anything else. The decided rules are in docs/api.md "Gateway".
//
// The same handler in public mode (NewPublic) is the public HTTPS seed of backlog 0043:
// Host must be the operator's domain or <cid>.<domain>, only roots marked public are
// served, and RASL answers carry CORS there. See public.go.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/target"
)

// Handler is the gateway HTTP handler.
type Handler struct {
	content Content
	mux     *http.ServeMux
	// pub is set on the public listener (NewPublic); nil on the loopback gateway.
	pub *PublicOptions
}

// New returns a gateway over content. A nil content serves only the root page and
// answers not_implemented for content routes.
func New(content Content) *Handler {
	h := &Handler{content: content, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /{$}", h.root)
	h.mux.HandleFunc("GET /tree/{cid}", h.tree)
	h.mux.HandleFunc("GET /tree/{cid}/{path...}", h.tree)
	h.mux.HandleFunc("GET /blob/{cid}", h.blob)
	h.mux.HandleFunc("GET /at/{did}", h.at)
	h.mux.HandleFunc("GET /at/{did}/{path...}", h.at)
	h.mux.HandleFunc("GET /.well-known/rasl/{cid}", h.rasl)
	h.mux.HandleFunc("/", h.notFound)
	return h
}

// request carries the per-request origin decision.
type request struct {
	// site is the root CID when the request arrived on a <cid>.localhost (or
	// <cid>.<domain>) origin.
	site string
	port string
	// pub is the public listener's options, nil on loopback.
	pub *PublicOptions
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Cookies are never set or read; a stray one is simply ignored.
	r.Header.Del("Cookie")
	rq, err := h.parseHost(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "method %s is not allowed", r.Method))
		return
	}
	// An empty segment is refused before the mux could clean and redirect it (G6).
	if strings.Contains(r.URL.Path, "//") {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid path %q", r.URL.Path))
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, rq))
	if rq.site != "" {
		h.site(w, r, rq)
		return
	}
	h.mux.ServeHTTP(w, r)
}

type ctxKey struct{}

func reqInfo(r *http.Request) request {
	rq, _ := r.Context().Value(ctxKey{}).(request)
	return rq
}

// parseHost validates Host. On loopback: a loopback name or address, or <cid>.localhost,
// always with this listener's port; anything else (a public name rebound to 127.0.0.1,
// another port) is refused. On the public listener: exactly the configured domain or
// <cid>.<domain> (P5).
func (h *Handler) parseHost(r *http.Request) (request, error) {
	if h.pub != nil {
		return h.pub.parseHost(r)
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	rq := request{port: port}
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, lport, err := net.SplitHostPort(la.String()); err == nil {
			rq.port = lport
			if port != lport {
				return rq, forbidden("host %q does not name this gateway's port %s", r.Host, lport)
			}
		}
	}
	if host == "localhost" {
		return rq, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return rq, nil
		}
		return rq, forbidden("host %q is not a loopback address", r.Host)
	}
	if label, ok := strings.CutSuffix(host, ".localhost"); ok && !strings.Contains(label, ".") && target.IsCID(label) {
		rq.site = label
		return rq, nil
	}
	return rq, forbidden("host %q is not localhost, a loopback address or <cid>.localhost", r.Host)
}

// allowed reports whether root may be served on this listener: always on loopback; on
// the public listener only when the operator marked it (or a root above it) public (P2).
// A refused root reads exactly like unheld content, so the listener reveals nothing about
// what the node holds privately.
func (h *Handler) allowed(root string) error {
	if h.pub == nil || h.pub.Serves(root) {
		return nil
	}
	return apiv0.Errorf(apiv0.CodeNotFound, "%s is not served here", root)
}

func forbidden(format string, args ...any) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeForbidden, format, args...).
		WithAction("open the gateway as http://localhost:<port>/ or http://<cid>.localhost:<port>/")
}

// writeError answers with a plain-text error (never HTML) and the code's HTTP status.
func writeError(w http.ResponseWriter, err error) {
	var ae *apiv0.Error
	if !errors.As(err, &ae) {
		ae = &apiv0.Error{Code: apiv0.CodeFailed, Message: err.Error()}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(apiv0.HTTPStatus(ae.Code))
	fmt.Fprintf(w, "%s: %s\n", ae.Code, ae.Message)
	if ae.Action != "" {
		fmt.Fprintf(w, "%s\n", ae.Action)
	}
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	if h.pub != nil {
		writeError(w, apiv0.Errorf(apiv0.CodeNotFound, "no such route").
			WithAction("use /tree/<cid>/<path>, /blob/<cid>, /.well-known/rasl/<cid> or "+h.pub.SiteURL("<cid>")))
		return
	}
	writeError(w, apiv0.Errorf(apiv0.CodeNotFound, "no such gateway route").
		WithAction("use /tree/<cid>/<path>, /blob/<cid>, /at/<did>/…, /.well-known/rasl/<cid> or http://<cid>.localhost:<port>/"))
}

var rootPage = template.Must(template.New("root").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Name}} gateway</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em}code{background:#eee;padding:0 .3em}</style>
<h1>{{.Name}} gateway</h1>
<p>This is the local {{.Name}} gateway ({{.Version}}). It serves verified content from your
node to your browser.</p>
<ul>
<li><code>/tree/&lt;cid&gt;/…</code> — listings and downloads of a tree</li>
<li><code>/blob/&lt;cid&gt;</code> — one blob, as a download</li>
<li><code>/at/&lt;did&gt;[@&lt;rev&gt;]/…</code> — a held atproto repository snapshot: collections, records as JSON</li>
<li><code>/.well-known/rasl/&lt;cid&gt;</code> — one held object's bytes (DASL RASL), as a download</li>
<li><code>http://&lt;cid&gt;.localhost:{{.Port}}/…</code> — a tree as a site, on its own origin</li>
</ul>
<p>Use <code>{{.Name}} status</code> on the command line to see what the node is doing.</p>
`))

func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", listingCSP)
	if h.pub != nil {
		w.Header().Set("Cache-Control", "no-store")
		_ = publicRootPage.Execute(w, struct{ Name, Version, Domain string }{nu.Name, nu.Version, h.pub.Domain})
		return
	}
	_ = rootPage.Execute(w, struct{ Name, Version, Port string }{nu.Name, nu.Version, reqInfo(r).port})
}

// Content routes.

func (h *Handler) tree(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !target.IsCID(cid) {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", cid))
		return
	}
	h.serve(w, r, cid, r.PathValue("path"), false)
}

func (h *Handler) blob(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !target.IsCID(cid) {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", cid))
		return
	}
	if err := h.allowed(cid); err != nil {
		writeError(w, err)
		return
	}
	info, err := h.resolve(r.Context(), cid, "")
	if err != nil {
		writeError(w, err)
		return
	}
	if info.Kind == KindDir {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a directory", cid).WithAction("use /tree/"+cid+"/"))
		return
	}
	// A blob is always a download, whatever the node says about it.
	h.serveFile(w, r, &Info{Kind: KindBlob, CID: info.CID, Name: info.CID, Size: info.Size}, false)
}

func (h *Handler) site(w http.ResponseWriter, r *http.Request, rq request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	h.serve(w, r, rq.site, p, true)
}

func (h *Handler) resolve(ctx context.Context, root, p string) (*Info, error) {
	if h.content == nil {
		return nil, apiv0.NotImplemented("gateway content")
	}
	return h.content.Resolve(ctx, root, p)
}

// serve resolves root/path and serves a listing, index.html (site mode) or a file.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, root, p string, site bool) {
	// Validate the request path before trimming: "//evil.example/" would otherwise pass
	// as "evil.example" and a directory of that name would redirect to a scheme-relative
	// URL on another host.
	if strings.Contains(r.URL.Path, "//") {
		writeError(w, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid path %q", r.URL.Path))
		return
	}
	p = strings.Trim(p, "/")
	if err := validatePath(p); err != nil {
		writeError(w, err)
		return
	}
	if err := h.allowed(root); err != nil {
		writeError(w, err)
		return
	}
	info, err := h.resolve(r.Context(), root, p)
	if err != nil {
		writeError(w, err)
		return
	}
	switch info.Kind {
	case KindDir:
		if !strings.HasSuffix(r.URL.Path, "/") {
			// Relative links in listings and sites need the trailing slash. The target is
			// always this origin's own path (never a Location from content).
			http.Redirect(w, r, r.URL.EscapedPath()+"/", http.StatusMovedPermanently)
			return
		}
		if site {
			ip := path.Join(p, "index.html")
			if idx, err := h.resolve(r.Context(), root, ip); err == nil && idx.Kind == KindFile {
				h.serveFile(w, r, idx, true)
				return
			}
		}
		h.serveListing(w, r, root, p, info, site)
	case KindFile, KindBlob:
		h.serveFile(w, r, info, site)
	case KindSymlink:
		writeError(w, apiv0.Errorf(apiv0.CodeNotSupported, "%s is a symlink; the gateway does not follow symlinks", p))
	default:
		writeError(w, apiv0.Errorf(apiv0.CodeFailed, "unknown content kind %q", info.Kind))
	}
}

// validatePath rejects traversal and empty segments; p has no surrounding slashes.
func validatePath(p string) error {
	if p == "" {
		return nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsRune(seg, 0) {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid path %q", p)
		}
	}
	return nil
}

// immutableCache marks CID-addressed responses as cacheable forever.
const immutableCache = "public, max-age=31536000, immutable"

// serveFile serves bytes with Range support. On the bare origin only inline-safe types
// are shown inline; everything else is a download with a link to the site origin.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, info *Info, site bool) {
	if h.content == nil {
		writeError(w, apiv0.NotImplemented("gateway content"))
		return
	}
	mt := "application/octet-stream"
	attachment := info.Kind == KindBlob
	if info.Kind == KindFile {
		mt = sanitizeMime(info.Mime, info.Name)
		if !site && !inlineSafe(mt) {
			mt = "application/octet-stream"
			attachment = true
		}
	}
	f, err := h.content.Open(r.Context(), info)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", mt)
	hdr.Set("Cache-Control", immutableCache)
	hdr.Set("ETag", `"`+info.CID+`"`)
	if attachment {
		hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(info.Name, info.CID)}))
	}
	if !site {
		if u := reqInfo(r).siteURL(r.URL.EscapedPath()); u != "" {
			hdr.Set("Link", "<"+u+">; rel=\"alternate\"")
		}
	}
	// An empty name keeps ServeContent from guessing a type by extension.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// siteURL maps a bare-origin /tree/<cid>/<path> URL (escaped form) to its site-origin
// URL: http://<cid>.localhost:<port>/<path> on loopback, https://<cid>.<domain>/<path> on
// the public listener.
func (rq request) siteURL(urlPath string) string {
	rest, ok := strings.CutPrefix(urlPath, "/tree/")
	if !ok {
		return ""
	}
	cid, p, _ := strings.Cut(rest, "/")
	if rq.pub != nil {
		return rq.pub.SiteURL(cid) + p
	}
	u := "http://" + cid + ".localhost"
	if rq.port != "" {
		u += ":" + rq.port
	}
	return u + "/" + p
}

func safeFilename(name, fallback string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == "/" || strings.ContainsAny(name, "\x00\r\n\"") {
		return fallback
	}
	return name
}

// sanitizeMime validates an untrusted MIME type, keeping only a utf-8 charset parameter,
// and falls back to the name's extension, then to application/octet-stream.
func sanitizeMime(m, name string) string {
	if m != "" {
		if mt, params, err := mime.ParseMediaType(m); err == nil && strings.Count(mt, "/") == 1 {
			if strings.HasPrefix(mt, "text/") && strings.EqualFold(params["charset"], "utf-8") {
				return mt + "; charset=utf-8"
			}
			return mt
		}
	}
	if ext := path.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return "application/octet-stream"
}

// inlineSafe reports whether a type may be rendered inline on the bare loopback origin:
// types that cannot run script or load other resources. Everything else (HTML, SVG, XML,
// JavaScript, PDF, unknown) is active or sniffable and becomes a download.
func inlineSafe(mt string) bool {
	base, _, _ := strings.Cut(mt, ";")
	base = strings.TrimSpace(base)
	switch base {
	case "text/plain", "text/css", "application/json", "application/octet-stream":
		return true
	case "image/svg+xml":
		return false
	}
	for _, prefix := range []string{"image/", "video/", "audio/", "font/"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// Listings.

const listingCSP = "default-src 'none'; style-src 'unsafe-inline'"

type listingRow struct {
	Name string
	Href string // empty when the entry is not linkable (symlink, unsafe name)
	Kind Kind
	Size int64
	CID  string
}

type listingData struct {
	Name    string
	Root    string
	Path    string
	CID     string
	Size    int64
	Site    bool
	SiteURL string
	Parent  bool
	Rows    []listingRow
}

var listingPage = template.Must(template.New("listing").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Name}}: {{.Root}}/{{.Path}}</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:60em;margin:2em auto;padding:0 1em}table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.2em .6em;border-bottom:1px solid #eee}td.n{text-align:right}code{font-size:.9em;word-break:break-all}</style>
<h1>{{.Root}}{{if .Path}}/{{.Path}}{{end}}</h1>
<p>directory <code>{{.CID}}</code>, {{.Size}} bytes below{{if not .Site}} · <a href="{{.SiteURL}}">open as site</a>{{end}}</p>
<table><tr><th>name</th><th>kind</th><th class="n">size</th><th>cid</th></tr>
{{if .Parent}}<tr><td><a href="../">../</a></td><td>dir</td><td></td><td></td></tr>{{end}}
{{range .Rows}}<tr><td>{{if .Href}}<a href="{{.Href}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td><td>{{.Kind}}</td><td class="n">{{.Size}}</td><td><code>{{.CID}}</code></td></tr>
{{end}}</table>
`))

func (h *Handler) serveListing(w http.ResponseWriter, r *http.Request, root, p string, info *Info, site bool) {
	data := listingData{Name: nu.Name, Root: root, Path: p, CID: info.CID, Size: info.Size, Site: site, Parent: p != ""}
	if !site {
		data.SiteURL = reqInfo(r).siteURL(r.URL.EscapedPath())
	}
	for e, err := range h.content.List(r.Context(), info) {
		if err != nil {
			writeError(w, err)
			return
		}
		row := listingRow{Name: e.Name, Kind: e.Kind, Size: e.Size, CID: e.CID}
		// Symlinks and unsafe names are shown but not linked.
		if e.Kind != KindSymlink && validatePath(e.Name) == nil && !strings.Contains(e.Name, "/") {
			row.Href = url.PathEscape(e.Name)
			if e.Kind == KindDir {
				row.Href += "/"
			}
		}
		data.Rows = append(data.Rows, row)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", listingCSP)
	hdr.Set("Cache-Control", immutableCache)
	if r.Method == http.MethodHead {
		return
	}
	_ = listingPage.Execute(w, data)
}
