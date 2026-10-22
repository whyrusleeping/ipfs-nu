package api

import (
	"maps"
	"net/http"
	"strconv"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// clusterRoutes serve node.ClusterNode: /cluster/… for the networked cluster and
// /datasets/… for the implicit local cluster (docs/cluster.md "Interfaces").
var clusterRoutes = []Route{
	{"GET", "/cluster"},
	{"POST", "/cluster/init"},
	{"POST", "/cluster/invites"},
	{"POST", "/cluster/join"},
	{"DELETE", "/cluster/members/{peer}"},
	{"GET", "/cluster/datasets"},
	{"POST", "/cluster/datasets"},
	{"GET", "/cluster/datasets/{name}"},
	{"POST", "/cluster/datasets/{name}/head"},
	{"GET", "/cluster/datasets/{name}/wait"},
	{"GET", "/datasets"},
	{"POST", "/datasets"},
	{"GET", "/datasets/{name}"},
	{"POST", "/datasets/{name}/head"},
	{"GET", "/datasets/{name}/wait"},
}

func init() { Routes = append(Routes, clusterRoutes...) }

func (s *Server) clusterHandlers() map[Route]handlerFunc {
	h := s.historyHandlers()
	maps.Copy(h, map[Route]handlerFunc{
		{"GET", "/cluster"}:                       s.clusterStatus,
		{"POST", "/cluster/init"}:                 s.clusterInit,
		{"POST", "/cluster/invites"}:              s.clusterInvite,
		{"POST", "/cluster/join"}:                 s.clusterJoin,
		{"DELETE", "/cluster/members/{peer}"}:     s.clusterMemberRemove,
		{"GET", "/cluster/datasets"}:              s.datasets(node.ScopeCluster),
		{"POST", "/cluster/datasets"}:             s.datasetCreate(node.ScopeCluster),
		{"GET", "/cluster/datasets/{name}"}:       s.dataset(node.ScopeCluster),
		{"GET", "/datasets"}:                      s.datasets(node.ScopeLocal),
		{"POST", "/datasets"}:                     s.datasetCreate(node.ScopeLocal),
		{"GET", "/datasets/{name}"}:               s.dataset(node.ScopeLocal),
		{"POST", "/cluster/datasets/{name}/head"}: s.datasetUpdate(node.ScopeCluster),
		{"GET", "/cluster/datasets/{name}/wait"}:  s.datasetWait(node.ScopeCluster),
		{"POST", "/datasets/{name}/head"}:         s.datasetUpdate(node.ScopeLocal),
		{"GET", "/datasets/{name}/wait"}:          s.datasetWait(node.ScopeLocal),
	})
	return h
}

func (s *Server) cluster() (node.ClusterNode, error) {
	c, ok := s.n.(node.ClusterNode)
	if !ok {
		return nil, apiv0.NotImplemented("clusters")
	}
	return c, nil
}

func (s *Server) clusterStatus(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cluster()
	if err != nil {
		return err
	}
	st, err := c.ClusterStatus(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (s *Server) clusterInit(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cluster()
	if err != nil {
		return err
	}
	var req apiv0.ClusterInitRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	info, err := c.ClusterInit(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) clusterInvite(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cluster()
	if err != nil {
		return err
	}
	var req apiv0.ClusterInviteRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	inv, err := c.ClusterInvite(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, inv)
	return nil
}

func (s *Server) clusterJoin(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cluster()
	if err != nil {
		return err
	}
	var req apiv0.ClusterJoinRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	info, err := c.ClusterJoin(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) clusterMemberRemove(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cluster()
	if err != nil {
		return err
	}
	md, err := c.ClusterMemberRemove(r.Context(), apiv0.ClusterMemberRemoveRequest{Peer: r.PathValue("peer")})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, md)
	return nil
}

func (s *Server) datasets(scope string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.cluster()
		if err != nil {
			return err
		}
		ds, err := c.Datasets(r.Context(), scope)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, ds)
		return nil
	}
}

func (s *Server) datasetCreate(scope string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.cluster()
		if err != nil {
			return err
		}
		var req apiv0.DatasetCreateRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		d, err := c.DatasetCreate(r.Context(), scope, req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, d)
		return nil
	}
}

func (s *Server) dataset(scope string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.cluster()
		if err != nil {
			return err
		}
		d, err := c.Dataset(r.Context(), scope, r.PathValue("name"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, d)
		return nil
	}
}

func (s *Server) datasetUpdate(scope string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.cluster()
		if err != nil {
			return err
		}
		var req apiv0.DatasetUpdateRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		d, err := c.DatasetUpdate(r.Context(), scope, r.PathValue("name"), req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, d)
		return nil
	}
}

// datasetWait answers with the wait result; an unsatisfied wait is an error (timeout or
// failed) whose details carry the result.
func (s *Server) datasetWait(scope string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.cluster()
		if err != nil {
			return err
		}
		q := r.URL.Query()
		req := apiv0.DatasetWaitRequest{Target: q.Get("target"), Condition: q.Get("condition"), Holders: q["holder"], Mode: q.Get("mode"), Timeout: q.Get("timeout")}
		if a := q.Get("acks"); a != "" {
			n, err := strconv.Atoi(a)
			if err != nil || n < 1 {
				return apiv0.Errorf(apiv0.CodeInvalidArgument, "acks must be a positive integer")
			}
			req.Acks = n
		}
		res, err := c.DatasetWait(r.Context(), scope, r.PathValue("name"), req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}
}
