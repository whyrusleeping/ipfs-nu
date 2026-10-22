// Command collgs-dash serves the read-only JSON API of the CollGS prototype dashboard
// (vmx's collab-cluster dashboard: /api/overview, /api/nodes, /api/node/<addr>,
// /api/dataset/<info_hash>, /api/transfers) from a Nu collection's holdings index, so
// that the prototype's dashboard frontend can be pointed at a Nu cluster.
//
// It is a pilot example, not part of the daemon: run it next to a member that runs the
// holdings index (cluster.index = true), against that member's loopback API.
//
//	go run ./scripts/pilot/collgs-dash -api 127.0.0.1:5171 -collection s2 \
//	    -names k51…=rolling-7d,k51…=europe -listen 127.0.0.1:8080 [-static ./dashboard/dist]
//
// Mapping (docs/collections.md "CollGS preview v1"): a dataset is a Nu item; its
// info_hash is the hex SHA-256 digest of the item's entry CID (64 hex digits, like a
// BitTorrent v2 info hash); full copies are fresh complete holders; a node is a member
// as the index reports it. Piece-level fields (pieces, cells, rates, disk, pressure) have
// no Nu counterpart and are zero, and transfers are not reported by this build.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/core/cid"
)

// backend is the part of the Nu API the shim reads (client.Client implements it).
type backend interface {
	CollectionStatus(ctx context.Context, name string) (*apiv0.CollectionStatus, error)
	CollectionItems(ctx context.Context, name string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error)
	CollectionItem(ctx context.Context, name, path string) (*apiv0.CollectionItem, error)
	DagGet(ctx context.Context, target string) (json.RawMessage, error)
}

type shim struct {
	nu    backend
	coll  string
	names map[string]string // PeerId → display name

	mu     sync.Mutex
	byHash map[string]string // info_hash → item path, from the item change stream
	cursor string
}

func main() {
	apiAddr := flag.String("api", "127.0.0.1:5171", "Nu API address (a member running the holdings index)")
	coll := flag.String("collection", "s2", "collection name")
	listen := flag.String("listen", "127.0.0.1:8080", "listen address")
	names := flag.String("names", "", "display names: peer=name,peer=name")
	static := flag.String("static", "", "directory of the dashboard frontend to serve at /")
	flag.Parse()
	s := &shim{nu: client.New(*apiAddr), coll: *coll, names: map[string]string{}, byHash: map[string]string{}, cursor: "0"}
	for _, kv := range strings.Split(*names, ",") {
		if p, n, ok := strings.Cut(kv, "="); ok {
			s.names[strings.TrimSpace(p)] = strings.TrimSpace(n)
		}
	}
	mux := s.routes()
	if *static != "" {
		files := http.FileServer(http.Dir(*static))
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deep links such as /dataset/<hash> serve the frontend's index.
			if strings.Contains(path.Base(r.URL.Path), ".") || r.URL.Path == "/" {
				files.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, path.Join(*static, "index.html"))
		}))
	}
	log.Printf("collgs-dash: serving %s from %s on http://%s", *coll, *apiAddr, *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (s *shim) routes() *http.ServeMux {
	mux := http.NewServeMux()
	handle := func(pattern string, fn func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			defer cancel()
			out, err := fn(r.WithContext(ctx))
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				code := http.StatusBadGateway
				if ae, ok := err.(*apiv0.Error); ok && ae.Code == apiv0.CodeNotFound {
					code = http.StatusNotFound
				}
				w.WriteHeader(code)
				out = map[string]string{"error": err.Error()}
			}
			_ = json.NewEncoder(w).Encode(out)
		})
	}
	handle("GET /api/overview", s.overview)
	handle("GET /api/nodes", s.nodes)
	handle("GET /api/node/{addr}", s.node)
	handle("GET /api/dataset/{hash}", s.dataset)
	handle("GET /api/transfers", func(*http.Request) (any, error) {
		return map[string]any{"ts": now(), "transfers": []any{}}, nil
	})
	return mux
}

func now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

func (s *shim) name(peer string) string {
	if n, ok := s.names[peer]; ok {
		return n
	}
	if len(peer) > 10 {
		return "…" + peer[len(peer)-8:]
	}
	return peer
}

// infoHash is the hex digest of an item's entry CID.
func infoHash(entry string) string {
	c, err := cid.Parse(entry)
	if err != nil {
		return ""
	}
	d := c.Multihash().Digest
	return hex.EncodeToString(d[:])
}

func itemName(p string) string { return path.Base(p) }

func limitParam(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

// items pages through an items query up to limit.
func (s *shim) items(ctx context.Context, q apiv0.CollectionItemsQuery, limit int) ([]apiv0.CollectionItem, error) {
	var out []apiv0.CollectionItem
	for len(out) < limit {
		q.Limit = min(apiv0.MaxPageLimit, limit-len(out))
		p, err := s.nu.CollectionItems(ctx, s.coll, q)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" || len(p.Items) == 0 {
			break
		}
		q.Cursor = p.NextCursor
	}
	s.mu.Lock()
	for _, it := range out {
		s.byHash[infoHash(it.Entry)] = it.Path
	}
	s.mu.Unlock()
	return out, nil
}

type spread struct {
	Name string  `json:"name"`
	Frac float64 `json:"frac"`
}

func (s *shim) summary(it apiv0.CollectionItem) map[string]any {
	full, downloading := 0, 0
	sp := []spread{}
	for _, h := range it.Holders {
		switch {
		case h.State == "complete" && h.Fresh:
			full++
			sp = append(sp, spread{s.name(h.Peer), 1})
		case h.State == "fetching":
			downloading++
			sp = append(sp, spread{s.name(h.Peer), 0})
		}
	}
	return map[string]any{
		"info_hash": infoHash(it.Entry), "name": itemName(it.Path), "path": it.Path,
		"total_size": it.Size, "piece_length": 0, "num_pieces": 0,
		"nodes_seen": len(it.Holders), "full_copies": full, "durable_copies": full, "min_avail": full, "redundancy": full,
		"total_stored": it.Size * uint64(full), "downloading": downloading, "seeding": full,
		"download_rate": 0, "upload_rate": 0, "spread": sp,
	}
}

func (s *shim) overview(r *http.Request) (any, error) {
	ctx := r.Context()
	st, err := s.nu.CollectionStatus(ctx, s.coll)
	if err != nil {
		return nil, err
	}
	if st.Index == nil {
		return nil, fmt.Errorf("the node at the API does not run the holdings index (set cluster.index = true)")
	}
	items, err := s.items(ctx, apiv0.CollectionItemsQuery{Rarest: true, Holders: true}, limitParam(r, 100, 2000))
	if err != nil {
		return nil, err
	}
	var stored uint64
	for _, m := range st.Index.Members {
		stored += m.Bytes
	}
	atRisk, rarest := 0, -1
	for k, n := range st.Index.Copies {
		c, _ := strconv.Atoi(k)
		if c < 2 {
			atRisk += n
		}
		if n > 0 && (rarest < 0 || c < rarest) {
			rarest = c
		}
	}
	ds := make([]map[string]any, 0, len(items))
	replicating := 0
	for _, it := range items {
		d := s.summary(it)
		if d["downloading"].(int) > 0 {
			replicating++
		}
		ds = append(ds, d)
	}
	return map[string]any{"ts": now(), "total": st.Items, "at_risk": atRisk, "rarest": max(rarest, 0), "stored": stored,
		"nodes": len(st.Index.Members), "replicating": replicating, "download_rate": 0, "matched": st.Items, "datasets": ds}, nil
}

func (s *shim) nodeInfo(m apiv0.CollectionMember) map[string]any {
	return map[string]any{"addr": m.Peer, "name": s.name(m.Peer), "version": "", "limited_by": []string{},
		"pressure": map[string]int{}, "datasets": m.Items, "complete": m.Items, "stored": m.Bytes,
		"download_rate": 0, "upload_rate": 0, "num_peers": 0, "disk_free": 0, "disk_total": 0,
		"fresh": m.Fresh, "behind": m.Behind}
}

func (s *shim) members(ctx context.Context) ([]apiv0.CollectionMember, error) {
	st, err := s.nu.CollectionStatus(ctx, s.coll)
	if err != nil {
		return nil, err
	}
	if st.Index == nil {
		return nil, fmt.Errorf("the node at the API does not run the holdings index (set cluster.index = true)")
	}
	return st.Index.Members, nil
}

func (s *shim) nodes(r *http.Request) (any, error) {
	ms, err := s.members(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, s.nodeInfo(m))
	}
	return map[string]any{"ts": now(), "version": "nu", "nodes": out}, nil
}

func (s *shim) node(r *http.Request) (any, error) {
	addr := r.PathValue("addr")
	ms, err := s.members(r.Context())
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.Peer != addr {
			continue
		}
		items, err := s.items(r.Context(), apiv0.CollectionItemsQuery{Holder: addr, Holders: true}, limitParam(r, 1000, 100_000))
		if err != nil {
			return nil, err
		}
		ts := make([]map[string]any, 0, len(items))
		for _, it := range items {
			complete := false
			for _, h := range it.Holders {
				complete = complete || (h.Peer == addr && h.State == "complete")
			}
			progress, stored := 0.0, uint64(0)
			if complete {
				progress, stored = 1, it.Size
			}
			ts = append(ts, map[string]any{"info_hash": infoHash(it.Entry), "name": itemName(it.Path), "path": it.Path,
				"total_size": it.Size, "complete": complete, "progress": progress, "stored": stored,
				"download_rate": 0, "upload_rate": 0, "num_peers": 0})
		}
		out := s.nodeInfo(m)
		out["ts"], out["matched"], out["torrents"] = now(), m.Items, ts
		return out, nil
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "no member %s", addr)
}

// lookup finds an item path by info_hash, following the item change stream for items
// the shim has not listed yet.
func (s *shim) lookup(ctx context.Context, hash string) (string, error) {
	s.mu.Lock()
	p, ok := s.byHash[hash]
	cursor := s.cursor
	s.mu.Unlock()
	if ok {
		return p, nil
	}
	for {
		page, err := s.nu.CollectionItems(ctx, s.coll, apiv0.CollectionItemsQuery{Since: cursor, Limit: apiv0.MaxPageLimit})
		if err != nil {
			return "", err
		}
		if page.Resync {
			cursor = "0"
			continue
		}
		s.mu.Lock()
		for _, it := range page.Items {
			if !it.Removed {
				s.byHash[infoHash(it.Entry)] = it.Path
			}
		}
		s.cursor = page.NextCursor
		p, ok = s.byHash[hash]
		s.mu.Unlock()
		if ok {
			return p, nil
		}
		if len(page.Items) == 0 {
			return "", apiv0.Errorf(apiv0.CodeNotFound, "no item with info_hash %s", hash)
		}
		cursor = page.NextCursor
	}
}

func (s *shim) dataset(r *http.Request) (any, error) {
	ctx := r.Context()
	hash := r.PathValue("hash")
	p, err := s.lookup(ctx, hash)
	if err != nil {
		return nil, err
	}
	it, err := s.nu.CollectionItem(ctx, s.coll, p)
	if err != nil {
		return nil, err
	}
	var meta json.RawMessage = []byte("null")
	if it.Meta != "" {
		if m, err := s.nu.DagGet(ctx, it.Meta); err == nil {
			meta = m
		}
	}
	sum := s.summary(*it)
	full := sum["full_copies"].(int)
	rows := []map[string]any{}
	holders := []string{}
	for _, h := range it.Holders {
		role, have, stored := "leech", 0, uint64(0)
		if h.State == "complete" {
			role, have, stored = "seed", 1, it.Size
			if h.Fresh {
				holders = append(holders, s.name(h.Peer))
			}
		}
		rows = append(rows, map[string]any{"name": s.name(h.Peer), "addr": h.Peer, "role": role, "have": have, "stored": stored,
			"num_peers": 0, "isolated": !h.Fresh, "cells": []float64{float64(have)}, "state": h.State, "src": h.Src})
	}
	return map[string]any{
		"ts": now(), "info_hash": hash, "name": itemName(p), "path": p, "num_pieces": 1, "piece_length": it.Size, "total_size": it.Size,
		"nodes_seen": len(it.Holders),
		"torrent":    map[string]any{"extras": map[string]any{"collab-cluster-torrentizer-metadata": meta}, "entry": it.Entry, "meta": it.Meta},
		"rows":       rows, "avail_cells": []int{full}, "histogram": []map[string]int{{"holders": full, "pieces": 1}},
		"files": []map[string]any{{"path": itemName(p), "size": it.Size, "full_copies": full, "full_holders": holders,
			"recon_copies": full, "partial": []string{}}},
		"summary": map[string]any{"full_copies": full, "full_holders": holders, "min_avail": full, "redundancy": float64(full),
			"fully_available": full > 0, "total_stored": it.Size * uint64(full)},
	}, nil
}
