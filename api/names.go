package api

import (
	"net/http"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// namesRoutes serve node.NamesNode (0045): login, public names, node registration and
// follows. They live on the loopback API only; sessions never leave the daemon.
var namesRoutes = []Route{
	{"POST", "/auth/login"},
	{"GET", "/auth/accounts"},
	{"DELETE", "/auth/accounts/{account}"},
	{"POST", "/names"},
	{"GET", "/names"},
	{"DELETE", "/names/{name}"},
	{"GET", "/resolve"},
	{"POST", "/node/register"},
	{"GET", "/node/devices"},
	{"POST", "/follows"},
	{"GET", "/follows"},
	{"DELETE", "/follows/{dataset}"},
	{"POST", "/follows:refresh"},
}

func init() { Routes = append(Routes, namesRoutes...) }

func (s *Server) namesHandlers() map[Route]handlerFunc {
	return map[Route]handlerFunc{
		{"POST", "/auth/login"}:                s.login,
		{"GET", "/auth/accounts"}:              s.accounts,
		{"DELETE", "/auth/accounts/{account}"}: s.logout,
		{"POST", "/names"}:                     s.publish,
		{"GET", "/names"}:                      s.names,
		{"DELETE", "/names/{name}"}:            s.unpublish,
		{"GET", "/resolve"}:              s.resolveName,
		{"POST", "/node/register"}:             s.nodeRegister,
		{"GET", "/node/devices"}:               s.devices,
		{"POST", "/follows"}:                   s.follow,
		{"GET", "/follows"}:                    s.follows,
		{"DELETE", "/follows/{dataset}"}:       s.unfollow,
		{"POST", "/follows:refresh"}:           s.followRefresh,
	}
}

func (s *Server) namesNode() (node.NamesNode, error) {
	nn, ok := s.n.(node.NamesNode)
	if !ok {
		return nil, apiv0.NotImplemented("names, login and follows")
	}
	return nn, nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	var req apiv0.LoginRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	out, err := nn.Login(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, out)
	return nil
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Accounts(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	acct := r.PathValue("account")
	if acct == "-" {
		acct = "" // the only logged-in account
	}
	if err := nn.Logout(r.Context(), acct); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	var req apiv0.PublishRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	out, err := nn.Publish(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) names(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Names(r.Context(), r.URL.Query().Get("account"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) unpublish(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Unpublish(r.Context(), r.PathValue("name"), r.URL.Query().Get("account"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) resolveName(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	t, err := targetParam(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	out, err := nn.ResolveName(r.Context(), t, apiv0.ResolveRequest{MaxAge: q.Get("max_age"), Fresh: q.Get("fresh") == "1", Offline: q.Get("offline") == "1"})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) nodeRegister(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	var req apiv0.NodeRegisterRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	out, err := nn.NodeRegister(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Devices(r.Context(), r.URL.Query().Get("account"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) follow(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	var req apiv0.FollowRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	out, err := nn.Follow(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) follows(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Follows(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) unfollow(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	out, err := nn.Unfollow(r.Context(), r.PathValue("dataset"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) followRefresh(w http.ResponseWriter, r *http.Request) error {
	nn, err := s.namesNode()
	if err != nil {
		return err
	}
	var req apiv0.FollowRefreshRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	out, err := nn.FollowRefresh(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
