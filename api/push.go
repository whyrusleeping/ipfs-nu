package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// Item push routes (docs/collections.md "Ingest", docs/api.md "Item pushes"). They are
// served on the local API and, for producers on other machines, on the authenticated
// producer listener (NewProducer).
var pushRoutes = []Route{
	{"POST", "/cluster/collections/{name}/items"},
	{"POST", "/cluster/collections/{name}/items:batch"},
	{"DELETE", "/cluster/collections/{name}/items/{path...}"},
	{"GET", "/cluster/collections/{name}/pushes"},
	{"POST", "/cluster/collections/{name}/uploads"},
}

// Batch bounds.
const (
	MaxBatchLines = 10000
	MaxBatchBody  = 64 << 20
)

func init() { Routes = append(Routes, pushRoutes...) }

func (s *Server) pushHandlers() map[Route]handlerFunc {
	return map[Route]handlerFunc{
		{"POST", "/cluster/collections/{name}/items"}:             s.collectionPush,
		{"POST", "/cluster/collections/{name}/items:batch"}:       s.collectionPushBatch,
		{"DELETE", "/cluster/collections/{name}/items/{path...}"}: s.collectionRemove,
		{"GET", "/cluster/collections/{name}/pushes"}:             s.collectionPushes,
		{"POST", "/cluster/collections/{name}/uploads"}:           s.collectionUpload,
	}
}

func (s *Server) pushNode() (node.PushNode, error) {
	p, ok := s.n.(node.PushNode)
	if !ok {
		return nil, apiv0.NotImplemented("item pushes")
	}
	return p, nil
}

// waitParam reads ?wait=written[&timeout=…]: 0 without a wait.
func waitParam(r *http.Request) (time.Duration, error) {
	q := r.URL.Query()
	switch q.Get("wait") {
	case "":
		return 0, nil
	case apiv0.PushWritten:
	default:
		return 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "wait must be %q", apiv0.PushWritten)
	}
	d := apiv0.DefaultPushWait
	if v := q.Get("timeout"); v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d <= 0 {
			return 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "timeout must be a positive duration such as 30s")
		}
	}
	return min(d, apiv0.MaxPushWait), nil
}

// requireType is requireJSON for the push routes' other body types. Like JSON, neither
// application/x-ndjson nor application/octet-stream is a CORS-safelisted type: a form
// cannot send it and a script that sets it needs the preflight A4 refuses.
func requireType(r *http.Request, want string) error {
	mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(mt) != want {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "request body must be sent as Content-Type: %s, not %q", want, mt)
	}
	return nil
}

func (s *Server) collectionPush(w http.ResponseWriter, r *http.Request) error {
	p, err := s.pushNode()
	if err != nil {
		return err
	}
	wait, err := waitParam(r)
	if err != nil {
		return err
	}
	var req apiv0.CollectionPushRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	res, err := p.CollectionPush(r.Context(), r.PathValue("name"), req, wait)
	if err != nil {
		return err
	}
	status := http.StatusAccepted
	if res.State == apiv0.PushWritten || res.State == apiv0.PushFailed {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
	return nil
}

func (s *Server) collectionPushBatch(w http.ResponseWriter, r *http.Request) error {
	p, err := s.pushNode()
	if err != nil {
		return err
	}
	if err := requireType(r, "application/x-ndjson"); err != nil {
		return err
	}
	sc := bufio.NewScanner(io.LimitReader(r.Body, MaxBatchBody))
	sc.Buffer(make([]byte, 64<<10), MaxJSONBody)
	var reqs []apiv0.CollectionPushRequest
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(reqs) == MaxBatchLines {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "a batch takes at most %d items", MaxBatchLines).WithAction("split it into several requests")
		}
		var req apiv0.CollectionPushRequest
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "line %d: malformed JSON: %v", len(reqs)+1, err)
		}
		reqs = append(reqs, req)
	}
	if err := sc.Err(); err != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "reading the batch: %v", err)
	}
	lines, err := p.CollectionPushBatch(r.Context(), r.PathValue("name"), reqs)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return nil
		}
	}
	return nil
}

func (s *Server) collectionRemove(w http.ResponseWriter, r *http.Request) error {
	p, err := s.pushNode()
	if err != nil {
		return err
	}
	wait, err := waitParam(r)
	if err != nil {
		return err
	}
	res, err := p.CollectionRemove(r.Context(), r.PathValue("name"), r.PathValue("path"), wait)
	return reply(w, res, err)
}

func (s *Server) collectionPushes(w http.ResponseWriter, r *http.Request) error {
	p, err := s.pushNode()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "limit must be a non-negative integer")
		}
	}
	page, err := p.CollectionPushes(r.Context(), r.PathValue("name"), q.Get("since"), limit)
	return reply(w, page, err)
}

func (s *Server) collectionUpload(w http.ResponseWriter, r *http.Request) error {
	p, err := s.pushNode()
	if err != nil {
		return err
	}
	if err := requireType(r, "application/octet-stream"); err != nil {
		return err
	}
	size := r.ContentLength
	if size < 0 {
		size = -1
	}
	res, err := p.CollectionUpload(r.Context(), r.PathValue("name"), r.Body, size)
	return reply(w, res, err)
}
