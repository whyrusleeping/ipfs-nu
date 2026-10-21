package api

import (
	"io"
	"net/http"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// atprotoRoutes serve node.AtprotoNode (0053): verified record fetch, repository
// snapshots and CAR import/export. at:// targets also work on /cat and /ls.
var atprotoRoutes = []Route{
	{"GET", "/at/record"},
	{"POST", "/at/snapshot"},
	{"POST", "/car/import"},
	{"GET", "/car"},
}

func init() { Routes = append(Routes, atprotoRoutes...) }

func (s *Server) atprotoHandlers() map[Route]handlerFunc {
	return map[Route]handlerFunc{
		{"GET", "/at/record"}:    s.atRecord,
		{"POST", "/at/snapshot"}: s.atSnapshot,
		{"POST", "/car/import"}:  s.carImport,
		{"GET", "/car"}:          s.carExport,
	}
}

func (s *Server) atproto() (node.AtprotoNode, error) {
	a, ok := s.n.(node.AtprotoNode)
	if !ok {
		return nil, apiv0.NotImplemented("atproto and CAR commands")
	}
	return a, nil
}

func (s *Server) atRecord(w http.ResponseWriter, r *http.Request) error {
	a, err := s.atproto()
	if err != nil {
		return err
	}
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	rec, err := a.AtRecord(r.Context(), t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

func (s *Server) atSnapshot(w http.ResponseWriter, r *http.Request) error {
	a, err := s.atproto()
	if err != nil {
		return err
	}
	var req apiv0.AtSnapshotRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	o, err := a.AtSnapshot(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, o)
	return nil
}

func (s *Server) carImport(w http.ResponseWriter, r *http.Request) error {
	a, err := s.atproto()
	if err != nil {
		return err
	}
	var req apiv0.CarImportRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	o, err := a.CarImport(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, o)
	return nil
}

// CarContentType is the media type of CAR responses.
const CarContentType = "application/vnd.ipld.car; version=1"

func (s *Server) carExport(w http.ResponseWriter, r *http.Request) error {
	a, err := s.atproto()
	if err != nil {
		return err
	}
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	rc, err := a.CarExport(r.Context(), t, r.URL.Query().Get("profile"))
	if err != nil {
		return err
	}
	defer rc.Close()
	w.Header().Set("Content-Type", CarContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return nil
}
