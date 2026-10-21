// Package api serves the versioned local HTTP API.
//
// Every response body is JSON except operation event streams (NDJSON) and cat (bytes).
// Errors use the single envelope {"error": {code, message, action?, details?}} from
// apiv0. The server is loopback-only: it rejects requests whose Host is not a loopback
// name with its own port, rejects state-changing requests from a foreign Origin or
// cross-site fetch, and never sends CORS headers, so a page served by the gateway (or any
// web page) cannot operate the daemon. GET never changes state.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

//go:embed openapi.json
var openAPI []byte

// OpenAPI returns the embedded OpenAPI document for v0 (served at apiv0.OpenAPIPath).
func OpenAPI() []byte { return openAPI }

// Limits on request handling.
const (
	// MaxJSONBody bounds JSON request bodies.
	MaxJSONBody = 1 << 20
	// DefaultWait and MaxWait bound one wait request; clients loop for longer waits.
	DefaultWait = 30 * time.Second
	MaxWait     = 5 * time.Minute
)

// Server is the API handler.
type Server struct {
	n   node.Node
	log *slog.Logger
	mux *http.ServeMux
}

// New returns a handler serving node n under apiv0.Prefix.
func New(n node.Node, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{n: n, log: log, mux: http.NewServeMux()}
	s.register()
	return s
}

// Route is one API route; Routes lists them all (the OpenAPI document must match).
type Route struct {
	Method  string
	Pattern string // path pattern with {name} segments, under apiv0.Prefix
}

// Routes is the v0 route table.
var Routes = []Route{
	{"GET", "/status"},
	{"GET", "/openapi.json"},
	{"GET", "/operations"},
	{"GET", "/operations/{id}"},
	{"GET", "/operations/{id}/events"},
	{"GET", "/operations/{id}/wait"},
	{"POST", "/operations/{id}/cancel"},
	{"POST", "/add"},
	{"GET", "/cat"},
	{"GET", "/ls"},
	{"GET", "/pins"},
	{"POST", "/pins"},
	{"DELETE", "/pins/{cid}"},
	{"GET", "/dag"},
	{"POST", "/dag"},
	{"POST", "/get"},
	{"POST", "/share"},
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (s *Server) register() {
	handlers := map[Route]handlerFunc{
		{"GET", "/status"}:                  s.status,
		{"GET", "/openapi.json"}:            s.openapi,
		{"GET", "/operations"}:              s.operations,
		{"GET", "/operations/{id}"}:         s.operation,
		{"GET", "/operations/{id}/events"}:  s.operationEvents,
		{"GET", "/operations/{id}/wait"}:    s.operationWait,
		{"POST", "/operations/{id}/cancel"}: s.operationCancel,
		{"POST", "/add"}:                    s.add,
		{"GET", "/cat"}:                     s.cat,
		{"GET", "/ls"}:                      s.ls,
		{"GET", "/pins"}:                    s.pins,
		{"POST", "/pins"}:                   s.pinAdd,
		{"DELETE", "/pins/{cid}"}:           s.pinRm,
		{"GET", "/dag"}:                     s.dagGet,
		{"POST", "/dag"}:                    s.dagPut,
		{"POST", "/get"}:                    s.get,
		{"POST", "/share"}:                  s.share,
	}
	paths := map[string]bool{}
	for _, rt := range Routes {
		h, ok := handlers[rt]
		if !ok {
			panic("api: no handler for route " + rt.Method + " " + rt.Pattern)
		}
		s.mux.HandleFunc(rt.Method+" "+apiv0.Prefix+rt.Pattern, s.wrap(h))
		if !paths[rt.Pattern] {
			paths[rt.Pattern] = true
			// Path-only fallback so other methods get an enveloped 405.
			s.mux.HandleFunc(apiv0.Prefix+rt.Pattern, s.wrap(methodNotAllowed))
		}
	}
	s.mux.HandleFunc("/", s.wrap(notFound))
}

func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, err)
		}
	}
}

// ServeHTTP applies the loopback protections, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if err := checkHost(r); err != nil {
		writeError(w, err)
		return
	}
	if err := checkOrigin(r); err != nil {
		writeError(w, err)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// checkHost requires a loopback Host whose port is the port this request arrived on.
// This defends against DNS rebinding: a public name resolving to 127.0.0.1 still carries
// the public name in Host.
func checkHost(r *http.Request) error {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if !isLoopbackHost(host) {
		return forbidden("host %q is not a loopback address", r.Host)
	}
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, lport, err := net.SplitHostPort(la.String()); err == nil && port != lport {
			return forbidden("host %q does not name this listener's port %s", r.Host, lport)
		}
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// checkOrigin rejects state-changing requests that a browser attributes to another
// origin or site. Loopback pages (the gateway) are a different origin and are rejected
// too: a fetched page must not gain daemon privileges.
func checkOrigin(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return nil
	case http.MethodOptions:
		return forbidden("cross-origin requests are not allowed")
	}
	if o := r.Header.Get("Origin"); o != "" {
		if !strings.EqualFold(o, "http://"+r.Host) {
			return forbidden("origin %q may not change daemon state", o)
		}
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return forbidden("%s requests may not change daemon state", sfs)
	}
	return nil
}

func forbidden(format string, args ...any) *apiv0.Error {
	return apiv0.Errorf(apiv0.CodeForbidden, format, args...).
		WithAction("use the " + nu.Name + " CLI or the client package on this machine; the API accepts only same-origin loopback requests")
}

func notFound(w http.ResponseWriter, r *http.Request) error {
	return apiv0.Errorf(apiv0.CodeNotFound, "no route %s %s", r.Method, r.URL.Path).
		WithAction("see the API description at " + apiv0.OpenAPIPath)
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "method %s is not allowed on %s", r.Method, r.URL.Path).
		WithAction("see the API description at " + apiv0.OpenAPIPath)
}

// writeError writes any error as the envelope. Non-apiv0 errors become code failed.
func writeError(w http.ResponseWriter, err error) {
	var ae *apiv0.Error
	switch {
	case errors.As(err, &ae):
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		ae = &apiv0.Error{Code: apiv0.CodeCancelled, Message: err.Error()}
	default:
		ae = &apiv0.Error{Code: apiv0.CodeFailed, Message: err.Error()}
	}
	writeJSON(w, apiv0.HTTPStatus(ae.Code), apiv0.Envelope{Error: ae})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(mt) != "application/json" {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "request body must be application/json, not %q", mt)
		}
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "malformed JSON body: %v", err)
	}
	return nil
}

// Handlers.

func (s *Server) status(w http.ResponseWriter, r *http.Request) error {
	st, err := s.n.Status(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write(openAPI)
	return err
}

func pageParams(r *http.Request) (cursor string, limit int, err error) {
	q := r.URL.Query()
	cursor = q.Get("cursor")
	if l := q.Get("limit"); l != "" {
		limit, err = strconv.Atoi(l)
		if err != nil || limit < 0 {
			return "", 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "limit must be a non-negative integer")
		}
	}
	return cursor, limit, nil
}

func (s *Server) operations(w http.ResponseWriter, r *http.Request) error {
	cursor, limit, err := pageParams(r)
	if err != nil {
		return err
	}
	page, err := s.n.Operations(r.Context(), cursor, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) error {
	o, err := s.n.Operation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, o)
	return nil
}

func (s *Server) operationEvents(w http.ResponseWriter, r *http.Request) error {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "since must be a non-negative event sequence number")
		}
		since = n
	}
	st, err := s.n.OperationEvents(r.Context(), r.PathValue("id"), since)
	if err != nil {
		return err
	}
	defer st.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush() // send headers now so the client sees the stream open
	enc := json.NewEncoder(w)
	for {
		ev, err := st.Next(r.Context())
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// The client went away or the server is shutting down; nothing to report.
			return nil
		}
		if err := enc.Encode(ev); err != nil {
			return nil
		}
		_ = rc.Flush()
	}
}

func (s *Server) operationWait(w http.ResponseWriter, r *http.Request) error {
	timeout := DefaultWait
	if v := r.URL.Query().Get("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "timeout must be a duration such as 30s")
		}
		timeout = d
	}
	if timeout == 0 || timeout > MaxWait {
		timeout = MaxWait
	}
	o, err := s.n.WaitOperation(r.Context(), r.PathValue("id"), timeout)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, o)
	return nil
}

func (s *Server) operationCancel(w http.ResponseWriter, r *http.Request) error {
	if err := s.n.CancelOperation(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	o, err := s.n.Operation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, o)
	return nil
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) error {
	var req apiv0.AddRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	o, err := s.n.Add(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, o)
	return nil
}

func targetParam(r *http.Request) (string, error) {
	t := r.URL.Query().Get("target")
	if t == "" {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "missing target query parameter")
	}
	return t, nil
}

func (s *Server) cat(w http.ResponseWriter, r *http.Request) error {
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	rc, err := s.n.Cat(r.Context(), t)
	if err != nil {
		return err
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return nil
}

func (s *Server) ls(w http.ResponseWriter, r *http.Request) error {
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	l, err := s.n.Ls(r.Context(), t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, l)
	return nil
}

func (s *Server) pins(w http.ResponseWriter, r *http.Request) error {
	cursor, limit, err := pageParams(r)
	if err != nil {
		return err
	}
	page, err := s.n.Pins(r.Context(), cursor, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

func (s *Server) pinAdd(w http.ResponseWriter, r *http.Request) error {
	var req apiv0.PinRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	o, err := s.n.PinAdd(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, o)
	return nil
}

func (s *Server) pinRm(w http.ResponseWriter, r *http.Request) error {
	if err := s.n.PinRm(r.Context(), r.PathValue("cid")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) dagGet(w http.ResponseWriter, r *http.Request) error {
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	doc, err := s.n.DagGet(r.Context(), t)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(doc)
	return err
}

func (s *Server) dagPut(w http.ResponseWriter, r *http.Request) error {
	res, err := s.n.DagPut(r.Context(), io.LimitReader(r.Body, MaxJSONBody))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) error {
	var req apiv0.GetRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	o, err := s.n.Get(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, o)
	return nil
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) error {
	var req apiv0.ShareRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	res, err := s.n.Share(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}
