package api

import (
	"net/http"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// publicRoutes serve node.PublicNode (0043): the roots the operator marks for the
// public HTTPS listener. They live on the loopback API only; the public listener itself
// has no API routes (P1).
var publicRoutes = []Route{
	{"GET", "/public"},
	{"POST", "/public"},
	{"DELETE", "/public/{cid}"},
}

func init() { Routes = append(Routes, publicRoutes...) }

func (s *Server) publicHandlers() map[Route]handlerFunc {
	return map[Route]handlerFunc{
		{"GET", "/public"}:          s.publicList,
		{"POST", "/public"}:         s.publicAdd,
		{"DELETE", "/public/{cid}"}: s.publicRm,
	}
}

func (s *Server) publicNode() (node.PublicNode, error) {
	p, ok := s.n.(node.PublicNode)
	if !ok {
		return nil, apiv0.NotImplemented("public roots")
	}
	return p, nil
}

func (s *Server) publicList(w http.ResponseWriter, r *http.Request) error {
	p, err := s.publicNode()
	if err != nil {
		return err
	}
	cursor, limit, err := pageParams(r)
	if err != nil {
		return err
	}
	page, err := p.PublicList(r.Context(), cursor, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

func (s *Server) publicAdd(w http.ResponseWriter, r *http.Request) error {
	p, err := s.publicNode()
	if err != nil {
		return err
	}
	var req apiv0.PublicRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	root, err := p.PublicAdd(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, root)
	return nil
}

func (s *Server) publicRm(w http.ResponseWriter, r *http.Request) error {
	p, err := s.publicNode()
	if err != nil {
		return err
	}
	if err := p.PublicRm(r.Context(), r.PathValue("cid")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
