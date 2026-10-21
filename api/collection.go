package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// collectionRoutes serve node.CollectionNode (docs/collections.md, docs/api.md
// "Collections").
var collectionRoutes = []Route{
	{"GET", "/cluster/collections/{name}"},
	{"GET", "/cluster/collections/{name}/items"},
	{"GET", "/cluster/collections/{name}/items/{path...}"},
	{"POST", "/cluster/collections/{name}/hold"},
	{"POST", "/cluster/collections/{name}/release"},
	{"GET", "/cluster/collections/{name}/rules"},
	{"PUT", "/cluster/collections/{name}/rules"},
	{"GET", "/cluster/collections/{name}/conflicts"},
	{"GET", "/cluster/metrics"},
}

func init() { Routes = append(Routes, collectionRoutes...) }

func (s *Server) collectionHandlers() map[Route]handlerFunc {
	return map[Route]handlerFunc{
		{"GET", "/cluster/collections/{name}"}:                 s.collectionStatus,
		{"GET", "/cluster/collections/{name}/items"}:           s.collectionItems,
		{"GET", "/cluster/collections/{name}/items/{path...}"}: s.collectionItem,
		{"POST", "/cluster/collections/{name}/hold"}:           s.collectionHold(true),
		{"POST", "/cluster/collections/{name}/release"}:        s.collectionHold(false),
		{"GET", "/cluster/collections/{name}/rules"}:           s.collectionRules,
		{"PUT", "/cluster/collections/{name}/rules"}:           s.collectionSetRules,
		{"GET", "/cluster/collections/{name}/conflicts"}:       s.collectionConflicts,
		{"GET", "/cluster/metrics"}:                            s.clusterMetrics,
	}
}

func (s *Server) collectionNode() (node.CollectionNode, error) {
	c, ok := s.n.(node.CollectionNode)
	if !ok {
		return nil, apiv0.NotImplemented("collections")
	}
	return c, nil
}

func reply[T any](w http.ResponseWriter, v T, err error) error {
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *Server) collectionStatus(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	st, err := c.CollectionStatus(r.Context(), r.PathValue("name"))
	return reply(w, st, err)
}

func boolParam(q map[string][]string, k string) (bool, error) {
	v := ""
	if vs := q[k]; len(vs) > 0 {
		v = vs[0]
	}
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s must be true or false", k)
	}
	return b, nil
}

func (s *Server) collectionItems(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	iq := apiv0.CollectionItemsQuery{Since: q.Get("since"), Cursor: q.Get("cursor"), Holder: q.Get("holder"), Prefix: q.Get("prefix")}
	if v := q.Get("limit"); v != "" {
		if iq.Limit, err = strconv.Atoi(v); err != nil || iq.Limit < 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "limit must be a non-negative integer")
		}
	}
	for k, p := range map[string]*bool{"rarest": &iq.Rarest, "unheld": &iq.Unheld, "held": &iq.Held} {
		if *p, err = boolParam(q, k); err != nil {
			return err
		}
	}
	if m := q.Get("match"); m != "" {
		if !json.Valid([]byte(m)) {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "match must be a JSON rule object")
		}
		iq.Match = json.RawMessage(m)
	}
	page, err := c.CollectionItems(r.Context(), r.PathValue("name"), iq)
	return reply(w, page, err)
}

func (s *Server) collectionItem(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	it, err := c.CollectionItem(r.Context(), r.PathValue("name"), r.PathValue("path"))
	return reply(w, it, err)
}

func (s *Server) collectionHold(hold bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.collectionNode()
		if err != nil {
			return err
		}
		var req apiv0.CollectionHoldRequest
		if err := readJSON(r, &req); err != nil {
			return err
		}
		it, err := c.CollectionHold(r.Context(), r.PathValue("name"), req.Path, hold)
		return reply(w, it, err)
	}
}

func (s *Server) collectionRules(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	rules, err := c.CollectionRules(r.Context(), r.PathValue("name"))
	return reply(w, rules, err)
}

func (s *Server) collectionSetRules(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	dry, err := boolParam(r.URL.Query(), "dry_run")
	if err != nil {
		return err
	}
	var req struct {
		Rules json.RawMessage `json:"rules"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	res, err := c.CollectionSetRules(r.Context(), r.PathValue("name"), req.Rules, dry)
	return reply(w, res, err)
}

func (s *Server) collectionConflicts(w http.ResponseWriter, r *http.Request) error {
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	cs, err := c.CollectionConflicts(r.Context(), r.PathValue("name"))
	return reply(w, cs, err)
}

// clusterMetrics renders collection aggregates in the Prometheus text format.
func (s *Server) clusterMetrics(w http.ResponseWriter, r *http.Request) error {
	cn, err := s.cluster()
	if err != nil {
		return err
	}
	c, err := s.collectionNode()
	if err != nil {
		return err
	}
	ds, err := cn.Datasets(r.Context(), node.ScopeCluster)
	if err != nil {
		return err
	}
	var b strings.Builder
	metric := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	type sample struct {
		labels string
		v      float64
	}
	series := map[string][]sample{}
	help := map[string]string{
		"nu_collection_items":            "Items in the merged catalog.",
		"nu_collection_bytes":            "Logical bytes of the merged catalog's items.",
		"nu_collection_conflicts":        "Union-merge conflicts.",
		"nu_collection_local_items":      "Items this node holds, by state.",
		"nu_collection_local_bytes":      "Logical bytes this node holds.",
		"nu_collection_shortfall_bytes":  "Logical bytes of matched items paused by rule budgets.",
		"nu_collection_holdings_log_seq": "This node's holdings-log position.",
		"nu_collection_unheld_items":     "Items without a fresh complete holder (holdings index).",
		"nu_collection_copies_items":     "Items by fresh complete-holder count (holdings index).",
		"nu_collection_member_items":     "Items each member reports complete (holdings index).",
	}
	add := func(name, labels string, v float64) { series[name] = append(series[name], sample{labels, v}) }
	for _, d := range ds {
		if d.Collection == nil {
			continue
		}
		st, err := c.CollectionStatus(r.Context(), d.Name)
		if err != nil {
			continue
		}
		l := `collection="` + d.Name + `"`
		add("nu_collection_items", l, float64(st.Items))
		add("nu_collection_bytes", l, float64(st.Bytes))
		add("nu_collection_conflicts", l, float64(st.Conflicts))
		for state, n := range st.Local {
			add("nu_collection_local_items", l+`,state="`+state+`"`, float64(n))
		}
		add("nu_collection_local_bytes", l, float64(st.LocalBytes))
		add("nu_collection_shortfall_bytes", l, float64(st.Shortfall))
		add("nu_collection_holdings_log_seq", l, float64(st.LogSeq))
		if st.Index != nil {
			add("nu_collection_unheld_items", l, float64(st.Index.Unheld))
			for k, n := range st.Index.Copies {
				add("nu_collection_copies_items", l+`,copies="`+k+`"`, float64(n))
			}
			for _, m := range st.Index.Members {
				add("nu_collection_member_items", l+`,peer="`+m.Peer+`"`, float64(m.Items))
			}
		}
	}
	names := make([]string, 0, len(help))
	for k := range help {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		ss := series[name]
		if len(ss) == 0 {
			continue
		}
		slices.SortFunc(ss, func(a, b sample) int { return strings.Compare(a.labels, b.labels) })
		metric(name, help[name])
		for _, smp := range ss {
			fmt.Fprintf(&b, "%s{%s} %g\n", name, smp.labels, smp.v)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, err = w.Write([]byte(b.String()))
	return err
}
