package api

import (
	"net/http"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// repairRoutes serve node.RepairNode (0048): member records, keys, invites, release and
// audits.
var repairRoutes = []Route{
	{"POST", "/cluster/members/{peer}"},
	{"POST", "/cluster/keys"},
	{"DELETE", "/cluster/keys/{did}"},
	{"DELETE", "/cluster/invites/{id}"},
	{"POST", "/cluster/audit"},
	{"POST", "/cluster/datasets/{name}/release"},
	{"POST", "/datasets/{name}/release"},
}

func init() { Routes = append(Routes, repairRoutes...) }

func (s *Server) repairHandlers() map[Route]handlerFunc {
	call := func(fn func(node.RepairNode, *http.Request) (any, error)) handlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			rn, ok := s.n.(node.RepairNode)
			if !ok {
				return apiv0.NotImplemented("cluster administration")
			}
			out, err := fn(rn, r)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, out)
			return nil
		}
	}
	release := func(scope string) handlerFunc {
		return call(func(rn node.RepairNode, r *http.Request) (any, error) {
			var req apiv0.ReleaseRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return rn.DatasetRelease(r.Context(), scope, r.PathValue("name"), req)
		})
	}
	return map[Route]handlerFunc{
		{"POST", "/cluster/members/{peer}"}: call(func(rn node.RepairNode, r *http.Request) (any, error) {
			var req apiv0.MemberSetRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			req.Peer = r.PathValue("peer")
			return rn.ClusterMemberSet(r.Context(), req)
		}),
		{"POST", "/cluster/keys"}: call(func(rn node.RepairNode, r *http.Request) (any, error) {
			var req apiv0.KeyRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return rn.ClusterKey(r.Context(), req)
		}),
		{"DELETE", "/cluster/keys/{did}"}: call(func(rn node.RepairNode, r *http.Request) (any, error) {
			return rn.ClusterKeyRevoke(r.Context(), r.PathValue("did"))
		}),
		{"DELETE", "/cluster/invites/{id}"}: call(func(rn node.RepairNode, r *http.Request) (any, error) {
			return rn.ClusterInviteRevoke(r.Context(), r.PathValue("id"))
		}),
		{"POST", "/cluster/audit"}: call(func(rn node.RepairNode, r *http.Request) (any, error) {
			var req apiv0.AuditRequest
			if err := readJSON(r, &req); err != nil {
				return nil, err
			}
			return rn.ClusterAudit(r.Context(), req)
		}),
		{"POST", "/cluster/datasets/{name}/release"}: release(node.ScopeCluster),
		{"POST", "/datasets/{name}/release"}:         release(node.ScopeLocal),
	}
}
