package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// fake is a holdings index with two items: one held by two members, one by one (and
// fetching on the other).
type fake struct{}

const (
	entryA = "bafyreihycsrj5a6gcj6x3z2mmzt5i3k7fqtq5dtgvlgdeqyddkbudnclwa"
	entryB = "bafyreiekingo3gpyqt7thdumcoz5vhjt7owgfi7fyytn7ca7bs7b4ye6ka"
)

var items = []apiv0.CollectionItem{
	{Path: "2026/10/21/S2B_B", Entry: entryB, Size: 20, Meta: "bafymeta", Holders: []apiv0.CollectionHolder{
		{Peer: "p1", State: "complete", Fresh: true}, {Peer: "p2", State: "fetching", Fresh: true}}},
	{Path: "2026/10/21/S2A_A", Entry: entryA, Size: 10, Holders: []apiv0.CollectionHolder{
		{Peer: "p1", State: "complete", Fresh: true}, {Peer: "p2", State: "complete", Fresh: true}}},
}

func (fake) CollectionStatus(context.Context, string) (*apiv0.CollectionStatus, error) {
	return &apiv0.CollectionStatus{Items: 2, Index: &apiv0.CollectionIndex{Copies: map[string]int{"1": 1, "2": 1},
		Members: []apiv0.CollectionMember{{Peer: "p1", Fresh: true, Items: 2, Bytes: 30}, {Peer: "p2", Fresh: true, Items: 1, Bytes: 10}}}}, nil
}

func (fake) CollectionItems(_ context.Context, _ string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error) {
	switch {
	case q.Since == "0":
		return &apiv0.Page[apiv0.CollectionItem]{Items: items, NextCursor: "c1"}, nil
	case q.Since != "":
		return &apiv0.Page[apiv0.CollectionItem]{Items: []apiv0.CollectionItem{}, NextCursor: q.Since}, nil
	case q.Holder == "p2":
		return &apiv0.Page[apiv0.CollectionItem]{Items: items}, nil
	}
	return &apiv0.Page[apiv0.CollectionItem]{Items: items}, nil
}

func (fake) CollectionItem(_ context.Context, _ string, path string) (*apiv0.CollectionItem, error) {
	for _, it := range items {
		if it.Path == path {
			return &it, nil
		}
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "no item")
}

func (fake) DagGet(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"stac": {"id": "S2B_B"}}`), nil
}

// TestShim: the prototype dashboard's routes answer from the index with copies,
// rarest-first order, holders by name, per-node items and per-dataset metadata.
func TestShim(t *testing.T) {
	s := &shim{nu: fake{}, coll: "s2", names: map[string]string{"p1": "rolling-7d"}, byHash: map[string]string{}, cursor: "0"}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	get := func(path string, out any) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", path, res.Status, err)
		}
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var ov struct {
		Total, Rarest, Nodes, Replicating int
		AtRisk                            int `json:"at_risk"`
		Datasets                          []struct {
			InfoHash   string `json:"info_hash"`
			Name       string
			FullCopies int `json:"full_copies"`
			Spread     []struct{ Name string }
		}
	}
	get("/api/overview?limit=2000", &ov)
	if ov.Total != 2 || ov.AtRisk != 1 || ov.Replicating != 1 || ov.Nodes != 2 || ov.Rarest != 1 || len(ov.Datasets) != 2 || ov.Datasets[0].FullCopies != 1 ||
		ov.Datasets[0].Name != "S2B_B" || len(ov.Datasets[0].InfoHash) != 64 || ov.Datasets[1].Spread[0].Name != "rolling-7d" {
		t.Fatalf("overview %+v", ov)
	}
	var node struct {
		Name     string
		Torrents []struct{ Complete bool }
	}
	get("/api/node/p2", &node)
	if len(node.Torrents) != 2 || node.Torrents[0].Complete || !node.Torrents[1].Complete {
		t.Fatalf("node %+v", node)
	}
	// A fresh shim finds a dataset by info_hash through the item stream.
	s.byHash = map[string]string{}
	var ds struct {
		Name    string
		Torrent struct {
			Extras map[string]struct{ Stac struct{ ID string } } `json:"extras"`
		}
		Summary struct {
			FullCopies  int      `json:"full_copies"`
			FullHolders []string `json:"full_holders"`
		}
	}
	get("/api/dataset/"+ov.Datasets[0].InfoHash, &ds)
	if ds.Name != "S2B_B" || ds.Torrent.Extras["collab-cluster-torrentizer-metadata"].Stac.ID != "S2B_B" ||
		ds.Summary.FullCopies != 1 || ds.Summary.FullHolders[0] != "rolling-7d" {
		t.Fatalf("dataset %+v", ds)
	}
	if res, _ := http.Get(srv.URL + "/api/dataset/00"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown dataset: %s", res.Status)
	}
}
