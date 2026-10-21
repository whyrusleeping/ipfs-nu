// Package gateway serves verified content to browsers.
//
// This build serves only a placeholder root page on the loopback listener; the content
// routes (/tree/<cid>/…, /blob/<cid>, …) land with the store.
package gateway

import (
	"html/template"
	"net/http"

	"github.com/whyrusleeping/nu"
)

// Handler is the gateway HTTP handler.
type Handler struct {
	mux *http.ServeMux
}

// New returns a gateway handler.
func New() *Handler {
	h := &Handler{mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /{$}", h.root)
	h.mux.HandleFunc("/", h.notYet)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mux.ServeHTTP(w, r)
}

var rootPage = template.Must(template.New("root").Parse(`<!doctype html>
<meta charset="utf-8">
<title>{{.Name}} gateway</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40em;margin:3em auto;padding:0 1em}code{background:#eee;padding:0 .3em}</style>
<h1>{{.Name}} gateway</h1>
<p>This is the local {{.Name}} gateway ({{.Version}}). It serves verified content from your
node to your browser.</p>
<p>Content routes are not available in this build yet. They will be:</p>
<ul>
<li><code>/tree/&lt;cid&gt;/…</code> — directories and files of a tree</li>
<li><code>/blob/&lt;cid&gt;</code> — one blob</li>
</ul>
<p>Use <code>{{.Name}} status</code> on the command line to see what the node is doing.</p>
`))

func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	_ = rootPage.Execute(w, struct{ Name, Version string }{nu.Name, nu.Version})
}

func (h *Handler) notYet(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "this gateway route is not available in this build; see / for the planned routes", http.StatusNotFound)
}
