package api

import (
	"net/http"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// historyRoutes serve node.HistoryNode for both scopes (docs/cluster.md "Version
// history"; 0047).
var historyRoutes = []Route{
	{"GET", "/datasets/{name}/versions"},
	{"GET", "/datasets/{name}/diff"},
	{"POST", "/datasets/{name}/label"},
	{"POST", "/datasets/{name}/rollback"},
	{"POST", "/datasets/{name}/prune"},
	{"GET", "/cluster/datasets/{name}/versions"},
	{"GET", "/cluster/datasets/{name}/diff"},
	{"POST", "/cluster/datasets/{name}/label"},
	{"POST", "/cluster/datasets/{name}/rollback"},
	{"POST", "/cluster/datasets/{name}/prune"},
}

func init() { Routes = append(Routes, historyRoutes...) }

func (s *Server) historyHandlers() map[Route]handlerFunc {
	out := map[Route]handlerFunc{}
	for _, scope := range []string{node.ScopeLocal, node.ScopeCluster} {
		pre := "/datasets/{name}/"
		if scope == node.ScopeCluster {
			pre = "/cluster" + pre
		}
		out[Route{"GET", pre + "versions"}] = s.historyCall(scope, func(h node.HistoryNode, r *http.Request, name string) (any, error) {
			return h.DatasetVersions(r.Context(), scope, name)
		})
		out[Route{"GET", pre + "diff"}] = s.historyCall(scope, func(h node.HistoryNode, r *http.Request, name string) (any, error) {
			q := r.URL.Query()
			return h.DatasetDiff(r.Context(), scope, name, q.Get("from"), q.Get("to"))
		})
		out[Route{"POST", pre + "label"}] = s.historyCall(scope, func(h node.HistoryNode, r *http.Request, name string) (any, error) {
			var req apiv0.VersionLabelRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return h.DatasetLabel(r.Context(), scope, name, req)
		})
		out[Route{"POST", pre + "rollback"}] = s.historyCall(scope, func(h node.HistoryNode, r *http.Request, name string) (any, error) {
			var req apiv0.RollbackRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return h.DatasetRollback(r.Context(), scope, name, req)
		})
		out[Route{"POST", pre + "prune"}] = s.historyCall(scope, func(h node.HistoryNode, r *http.Request, name string) (any, error) {
			var req apiv0.PruneRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return h.DatasetPrune(r.Context(), scope, name, req)
		})
	}
	return out
}

func (s *Server) historyCall(scope string, fn func(node.HistoryNode, *http.Request, string) (any, error)) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		h, ok := s.n.(node.HistoryNode)
		if !ok {
			return apiv0.NotImplemented("version history")
		}
		out, err := fn(h, r, r.PathValue("name"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
}
