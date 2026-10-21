package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// Collections (docs/collections.md): `nu cluster collection …` and `nu cluster where`.

func init() {
	cmdCluster.sub = append(cmdCluster.sub, cmdCollection, cmdClusterWhere)
}

func (e *env) collections(ctx context.Context) (node.CollectionNode, error) {
	n, err := e.nodeOrLocal(ctx)
	if err != nil {
		return nil, err
	}
	c, ok := n.(node.CollectionNode)
	if !ok {
		return nil, apiv0.NotImplemented("collections")
	}
	return c, nil
}

// readTOMLOrJSON decodes a .json file as JSON and anything else as TOML into a generic map.
func readTOMLOrJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		err = json.Unmarshal(b, &out)
	} else {
		_, err = toml.Decode(string(b), &out)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return out, nil
}

// readFacets reads facet declarations: a table per facet ({ptr, type}), optionally under
// a top-level `facets` key.
func readFacets(path string) (map[string]apiv0.FacetDecl, error) {
	m, err := readTOMLOrJSON(path)
	if err != nil {
		return nil, err
	}
	if f, ok := m["facets"].(map[string]any); ok {
		m = f
	}
	b, _ := json.Marshal(m)
	var out map[string]apiv0.FacetDecl
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: facets must be tables {ptr, type}: %v", path, err)
	}
	return out, nil
}

// readRules reads a rules file: TOML `[[rule]]` tables, or a JSON list of rule objects
// (or {"rules": [...]}).
func readRules(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		var wrapped struct {
			Rules json.RawMessage `json:"rules"`
		}
		if json.Unmarshal(b, &wrapped) == nil && len(wrapped.Rules) > 0 {
			return wrapped.Rules, nil
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("%s: invalid JSON", path)
		}
		return b, nil
	}
	var doc struct {
		Rule []map[string]any `toml:"rule"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if doc.Rule == nil {
		doc.Rule = []map[string]any{}
	}
	return json.Marshal(doc.Rule)
}

// facetFilter turns repeated `--facet name.op=value` flags into a match rule: ops eq,
// in (comma list), min, max, within, intersects (w,s,e,n).
func facetFilter(fs []string) (json.RawMessage, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	rule := map[string]map[string]any{}
	for _, f := range fs {
		key, val, ok := strings.Cut(f, "=")
		name, op, ok2 := strings.Cut(key, ".")
		if !ok || !ok2 {
			return nil, fmt.Errorf("bad --facet %q (want name.op=value, e.g. cloud.max=30)", f)
		}
		if rule[name] == nil {
			rule[name] = map[string]any{}
		}
		switch op {
		case "eq", "within":
			rule[name][op] = val
		case "in":
			rule[name][op] = strings.Split(val, ",")
		case "min", "max":
			var n json.Number
			if json.Unmarshal([]byte(val), &n) == nil {
				rule[name][op] = n
			} else {
				rule[name][op] = val
			}
		case "intersects":
			var box []float64
			if err := json.Unmarshal([]byte("["+val+"]"), &box); err != nil || len(box) != 4 {
				return nil, fmt.Errorf("bad --facet %q: intersects takes west,south,east,north", f)
			}
			rule[name][op] = box
		default:
			return nil, fmt.Errorf("bad --facet %q: unknown op %q", f, op)
		}
	}
	return json.Marshal(rule)
}

func printItem(w io.Writer, it *apiv0.CollectionItem) {
	fmt.Fprintf(w, "%s\n  entry %s (%s, %d B), meta %s, writer %s\n", it.Path, it.Entry, it.Kind, it.Size, it.Meta, it.Writer)
	keys := make([]string, 0, len(it.Facets))
	for k := range it.Facets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  %s = %v\n", k, it.Facets[k])
	}
	if it.Copies != nil {
		fmt.Fprintf(w, "  copies %d (fresh complete holders), %d with stale heartbeats\n", *it.Copies, *it.Unknown)
	}
	if it.Local != "" {
		fmt.Fprintf(w, "  this node: %s\n", it.Local)
	}
	for _, h := range it.Holders {
		fresh := "fresh"
		if !h.Fresh {
			fresh = "stale"
		}
		fmt.Fprintf(w, "  holder %s %s (%s, %s)\n", h.Peer, h.State, h.Src, fresh)
	}
	for _, c := range it.Conflicts {
		fmt.Fprintf(w, "  conflict: %s lost to %s (%s): %s\n", c.Loser, c.Winner, c.Reason, c.Entry)
	}
}

func printRules(w io.Writer, r *apiv0.CollectionRules) {
	var rules []json.RawMessage
	_ = json.Unmarshal(r.Rules, &rules)
	if len(rules) == 0 {
		fmt.Fprintln(w, "no rules: this node holds no items by selection")
	}
	for i, rule := range rules {
		fmt.Fprintf(w, "%d. %s\n", i+1, rule)
		if i < len(r.DryRun) {
			d := r.DryRun[i]
			verb := "holds"
			switch {
			case d.Rescue:
				verb = "rescues among"
			case !d.Hold:
				verb = "excludes"
			}
			fmt.Fprintf(w, "   %s %d item(s), %d B (now held: %d)", verb, d.Items, d.Bytes, d.CurrentItems)
			if d.PausedItems > 0 {
				fmt.Fprintf(w, "; budget leaves out %d item(s), %d B", d.PausedItems, d.PausedBytes)
			}
			fmt.Fprintln(w)
		}
	}
	if !r.Applied {
		fmt.Fprintln(w, "(not applied)")
	}
}

func printCollectionStatus(w io.Writer, st *apiv0.CollectionStatus) {
	fmt.Fprintf(w, "collection %s (merge %s, replicas %d): %d item(s), %d B, %d conflict(s)\n", st.Name, st.Merge, st.Replicas, st.Items, st.Bytes, st.Conflicts)
	fmt.Fprintf(w, "  merged root %s\n", st.Root)
	for _, wr := range st.Writers {
		sync := "merged"
		if !wr.Synced {
			sync = "syncing"
		}
		fmt.Fprintf(w, "  writer %s head %s (%s)\n", wr.DID, wr.Head, sync)
	}
	for k, n := range st.FacetAbsent {
		if n > 0 {
			fmt.Fprintf(w, "  facet %s absent on %d item(s)\n", k, n)
		}
	}
	fmt.Fprintf(w, "  this node: %d rule(s), holdings %v, %d B, shortfall %d B, log %s:%d\n", st.Rules, st.Local, st.LocalBytes, st.Shortfall, st.LogEpoch, st.LogSeq)
	if ix := st.Index; ix != nil {
		fmt.Fprintf(w, "  index: %d unheld item(s), copies %v\n", ix.Unheld, ix.Copies)
		for _, m := range ix.Members {
			fresh := "fresh"
			if !m.Fresh {
				fresh = "stale"
			}
			fmt.Fprintf(w, "    %s %s: %d item(s), %d B, log %s:%d\n", m.Peer, fresh, m.Items, m.Bytes, m.Epoch, m.Seq)
		}
	}
	for _, warn := range st.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
}

func whereRun(ctx context.Context, e *env, ds, path string) error {
	c, err := e.collections(ctx)
	if err != nil {
		return err
	}
	it, err := c.CollectionItem(ctx, ds, path)
	if err != nil {
		return err
	}
	return e.emit(it, func(w io.Writer) { printItem(w, it) })
}

var cmdClusterWhere = &command{
	name:    "where",
	args:    "<collection>/<item path>",
	summary: "show an item's holders, copies and conflicts",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		ds, path, ok := strings.Cut(fs.Arg(0), "/")
		if fs.NArg() != 1 || !ok || path == "" {
			return usagef("cluster where needs <collection>/<item path>")
		}
		return whereRun(ctx, e, ds, path)
	},
}

func oneName(fs interface{ NArg() int }, what string) error {
	if fs.NArg() != 1 {
		return usagef("%s needs a collection name", what)
	}
	return nil
}

var cmdCollection = &command{
	name:    "collection",
	summary: "collections: items, holdings and this node's selection rules",
	sub: []*command{
		{
			name:    "status",
			args:    "<collection>",
			summary: "merged catalog, writers, local holdings and the holdings index",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if err := oneName(fs, "collection status"); err != nil {
					return err
				}
				c, err := e.collections(ctx)
				if err != nil {
					return err
				}
				st, err := c.CollectionStatus(ctx, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(st, func(w io.Writer) { printCollectionStatus(w, st) })
			},
		},
		{
			name:    "items",
			args:    "<collection> [--since <cursor>|0] [--rarest] [--holder <peer>] [--unheld] [--held] [--prefix p] [--facet name.op=value]...",
			summary: "list items, rarest first or filtered, or follow the item change stream with --since",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				var q apiv0.CollectionItemsQuery
				fs.StringVar(&q.Since, "since", "", "item change stream after this cursor (0: from the start)")
				fs.StringVar(&q.Cursor, "cursor", "", "page cursor")
				fs.IntVar(&q.Limit, "limit", 0, "page size (default 100, max 1000)")
				fs.BoolVar(&q.Rarest, "rarest", false, "rarest first (needs the holdings index)")
				fs.StringVar(&q.Holder, "holder", "", "items the index reports held by this peer")
				fs.BoolVar(&q.Unheld, "unheld", false, "items without a fresh complete holder")
				fs.BoolVar(&q.Held, "held", false, "items this node holds")
				fs.StringVar(&q.Prefix, "prefix", "", "items at or below this path")
				var facets []string
				fs.Func("facet", "facet predicate name.op=value (eq, in, min, max, within, intersects)", func(s string) error { facets = append(facets, s); return nil })
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if err := oneName(fs, "collection items"); err != nil {
					return err
				}
				m, err := facetFilter(facets)
				if err != nil {
					return usagef("%v", err)
				}
				q.Match = m
				c, err := e.collections(ctx)
				if err != nil {
					return err
				}
				p, err := c.CollectionItems(ctx, fs.Arg(0), q)
				if err != nil {
					return err
				}
				return e.emit(p, func(w io.Writer) {
					for _, it := range p.Items {
						copies := "-"
						if it.Copies != nil {
							copies = fmt.Sprint(*it.Copies)
						}
						local := it.Local
						if it.Removed {
							local = "removed"
						} else if local == "" {
							local = "-"
						}
						fmt.Fprintf(w, "%s\tcopies %s\tlocal %s\t%d B\n", it.Path, copies, local, it.Size)
					}
					if p.Resync {
						fmt.Fprintln(e.stderr, "the cursor expired: list again from --since 0")
					}
					if p.NextCursor != "" {
						fmt.Fprintf(e.stderr, "next: --%s %s\n", map[bool]string{true: "since", false: "cursor"}[q.Since != ""], p.NextCursor)
					}
				})
			},
		},
		{
			name:    "where",
			args:    "<collection> <item path>",
			summary: "show an item's holders, copies and conflicts",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 2 {
					return usagef("collection where needs <collection> <item path>")
				}
				return whereRun(ctx, e, fs.Arg(0), fs.Arg(1))
			},
		},
		{
			name:    "rules",
			args:    "<collection> [--set rules.toml|rules.json] [--dry-run]",
			summary: "show this node's selection rules and what they hold, or replace them (--dry-run only reports)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				set := fs.String("set", "", "rules file: TOML [[rule]] tables or a JSON list")
				dry := fs.Bool("dry-run", false, "report what the rules would hold without applying them")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if err := oneName(fs, "collection rules"); err != nil {
					return err
				}
				c, err := e.collections(ctx)
				if err != nil {
					return err
				}
				var res *apiv0.CollectionRules
				if *set == "" {
					if *dry {
						return usagef("--dry-run needs --set <file>")
					}
					res, err = c.CollectionRules(ctx, fs.Arg(0))
				} else {
					rules, rerr := readRules(*set)
					if rerr != nil {
						return usagef("%v", rerr)
					}
					res, err = c.CollectionSetRules(ctx, fs.Arg(0), rules, *dry)
				}
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) { printRules(w, res) })
			},
		},
		holdCommand(true),
		holdCommand(false),
		{
			name:    "conflicts",
			args:    "<collection>",
			summary: "list union-merge conflicts (entries that lost to an earlier-ranked writer)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if err := oneName(fs, "collection conflicts"); err != nil {
					return err
				}
				c, err := e.collections(ctx)
				if err != nil {
					return err
				}
				cs, err := c.CollectionConflicts(ctx, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(cs, func(w io.Writer) {
					if len(cs) == 0 {
						fmt.Fprintln(w, "no conflicts")
					}
					for _, x := range cs {
						fmt.Fprintf(w, "%s\t%s lost to %s (%s)\t%s\n", x.Path, x.Loser, x.Winner, x.Reason, x.Entry)
					}
				})
			},
		},
	},
}

func holdCommand(hold bool) *command {
	name, summary := "release", "clear this node's manual hold of an item"
	if hold {
		name, summary = "hold", "hold an item on this node regardless of rules (pin source collection-manual)"
	}
	return &command{
		name:    name,
		args:    "<collection> <item path>",
		summary: summary,
		run: func(ctx context.Context, e *env, args []string) error {
			fs := e.flags()
			if err := parseFlags(fs, args); err != nil {
				return err
			}
			if fs.NArg() != 2 {
				return usagef("collection %s needs <collection> <item path>", name)
			}
			c, err := e.collections(ctx)
			if err != nil {
				return err
			}
			it, err := c.CollectionHold(ctx, fs.Arg(0), fs.Arg(1), hold)
			if err != nil {
				return err
			}
			return e.emit(it, func(w io.Writer) { printItem(w, it) })
		},
	}
}
