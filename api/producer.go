package api

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// ProducerConfig configures the producer listener (rule A8 of docs/operations.md).
type ProducerConfig struct {
	// Hosts are the Host header values the listener answers to (A2 with a configured
	// allowlist in place of loopback): "name" matches any port, "name:port" exactly.
	Hosts []string
	// Authorize returns the collections a presented bearer token may push into.
	Authorize func(token string) (collections []string, ok bool)
}

// producerRoutes are served on the producer listener: the push routes plus the
// single-item status a producer reads outcomes from.
var producerRoutes = append([]Route{{"GET", "/cluster/collections/{name}/items/{path...}"}}, pushRoutes...)

// Producer serves the item push routes for producers on other machines. Every request
// needs a bearer token (Authorization header only) whose scope names the collection in
// the path; A3–A7 apply as on the local API, so a token never authorizes a
// browser-originated cross-site request. It never serves anything else of the API.
type Producer struct {
	s   *Server
	cfg ProducerConfig
	mux *http.ServeMux
}

// NewProducer returns the producer handler for node n.
func NewProducer(n node.Node, cfg ProducerConfig, log *slog.Logger) *Producer {
	p := &Producer{s: New(n, log), cfg: cfg, mux: http.NewServeMux()}
	handlers := p.s.pushHandlers()
	handlers[Route{"GET", "/cluster/collections/{name}/items/{path...}"}] = p.s.collectionItem
	for _, rt := range producerRoutes {
		h := handlers[rt]
		p.mux.HandleFunc(rt.Method+" "+apiv0.Prefix+rt.Pattern, p.s.wrap(p.scoped(h)))
	}
	p.mux.HandleFunc("/", p.s.wrap(notFound))
	return p
}

func (p *Producer) hostAllowed(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = strings.Trim(host, "[]")
	}
	for _, a := range p.cfg.Hosts {
		if _, _, err := net.SplitHostPort(a); err == nil {
			if strings.EqualFold(a, host) {
				return true
			}
		} else if strings.EqualFold(strings.Trim(a, "[]"), h) {
			return true
		}
	}
	return false
}

func (p *Producer) scoped(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			return unauthorized("this listener needs a bearer token in the Authorization header")
		}
		cols, ok := p.cfg.Authorize(strings.TrimSpace(tok))
		if !ok {
			return unauthorized("the bearer token is not valid (revoked, rotated or mistyped)")
		}
		name := r.PathValue("name")
		for _, c := range cols {
			if c == name {
				// The push is a remote producer's: no local paths, only its own uploads
				// (0077). The token's hash identifies it (a rotated token starts afresh).
				sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
				return h(w, r.WithContext(node.WithProducer(r.Context(), hex.EncodeToString(sum[:16]))))
			}
		}
		return apiv0.Errorf(apiv0.CodeForbidden, "the token may not push into collection %q", name).
			WithAction("create a token for it with `" + nu.Name + " api token create <name> --collection " + name + "`")
	}
}

func unauthorized(msg string) error {
	return apiv0.Errorf(apiv0.CodeForbidden, "%s", msg).
		WithAction("ask the node's operator for a push token (`" + nu.Name + " api token create`) and send it as Authorization: Bearer <token>")
}

// ServeHTTP applies the host allowlist and the origin rules, then routes.
func (p *Producer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if !p.hostAllowed(r.Host) {
		writeError(w, apiv0.Errorf(apiv0.CodeForbidden, "host %q is not one of this listener's names", r.Host).
			WithAction("use a name listed in api.producer.hosts"))
		return
	}
	if err := checkOrigin(r); err != nil {
		writeError(w, err)
		return
	}
	p.mux.ServeHTTP(w, r)
}
