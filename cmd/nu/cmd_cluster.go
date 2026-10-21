package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
)

// clusterNode returns the node as a node.ClusterNode (both the client and node.Local are).
func clusterNode(n node.Node) (node.ClusterNode, error) {
	c, ok := n.(node.ClusterNode)
	if !ok {
		return nil, apiv0.NotImplemented("clusters")
	}
	return c, nil
}

func (e *env) clusterLocal(ctx context.Context) (node.ClusterNode, error) {
	n, err := e.nodeOrLocalWrite(ctx)
	if err != nil {
		return nil, err
	}
	return clusterNode(n)
}

func (e *env) clusterNet(ctx context.Context) (node.ClusterNode, error) {
	n, err := e.nodeOrLocal(ctx)
	if err != nil {
		return nil, err
	}
	return clusterNode(n)
}

// parseSize parses a byte count with an optional K, M, G, T or P suffix (powers of 1024).
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	if i := strings.IndexByte("KMGTP", strings.ToUpper(s[len(s)-1:])[0]); i >= 0 {
		mult = int64(1) << (10 * (i + 1))
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return v * mult, nil
}

func parseTags(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad tag %q (want key=value)", kv)
		}
		out[k] = v
	}
	return out, nil
}

func printMetadata(w io.Writer, md apiv0.MetadataState) {
	if md.Commit == "" {
		return
	}
	state := "acknowledged"
	if !md.Acknowledged {
		state = "NOT yet acknowledged"
	}
	fmt.Fprintf(w, "metadata: %s by %d of %d storage member(s) (commit %s rev %s)\n", state, len(md.AckedBy), md.Required, md.Commit, md.Rev)
}

func printClusterInfo(w io.Writer, label string, c *apiv0.ClusterInfo) {
	fmt.Fprintf(w, "%s cluster %q (%s mode)\n  id   %s\n  self %s", label, c.Name, c.Mode, c.ID, c.Self)
	if !c.Member {
		fmt.Fprint(w, " (not a member)")
	}
	fmt.Fprintf(w, "\n  datasets %d, live invites %d, ignored records %d, held back %d\n", c.Datasets, c.Invites, c.Ignored, c.HeldBack)
	if len(c.Members) > 0 {
		fmt.Fprintln(w, "  members:")
		for _, m := range c.Members {
			conn := "disconnected"
			if m.Self {
				conn = "self"
			} else if m.Connected {
				conn = "connected"
			}
			hb := "no heartbeat"
			if m.LastHeartbeat != nil {
				hb = fmt.Sprintf("heartbeat %s ago, skew %dms, used %d B", time.Since(*m.LastHeartbeat).Round(time.Second), m.SkewMS, m.Used)
			}
			fmt.Fprintf(w, "    %s %s %s %s, %s\n", m.Peer, strings.Join(m.Roles, ","), m.State, conn, hb)
		}
	}
	fmt.Fprintln(w, "  keys:")
	for _, k := range c.Keys {
		extra := ""
		if k.Held {
			extra += " (held here)"
		}
		if k.Revoked {
			extra += " (revoked)"
		}
		fmt.Fprintf(w, "    %-6s %s%s\n", k.Role, k.DID, extra)
	}
	for _, warn := range c.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
}

func printDataset(w io.Writer, d *apiv0.Dataset) {
	fmt.Fprintf(w, "%s (%s cluster) replicas %d, record by %s\n", d.Name, d.Cluster, d.Replicas, d.Signer)
	if c := d.Collection; c != nil {
		fmt.Fprintf(w, "  collection: merge %s, %d facet(s)", c.Merge, len(c.Facets))
		if c.Expire != "" {
			fmt.Fprintf(w, ", expire %s", c.Expire)
		}
		if c.Ingest != nil && len(c.Ingest.Hosts) > 0 {
			fmt.Fprintf(w, ", ingest hosts %s", strings.Join(c.Ingest.Hosts, ", "))
		}
		fmt.Fprintln(w)
	}
	if d.Head != nil {
		fmt.Fprintf(w, "  head %s (%s) by %s\n", d.Head.Target, d.Head.Kind, d.Head.Signer)
	}
	if len(d.Heads) > 1 {
		for _, h := range d.Heads {
			fmt.Fprintf(w, "  writer head %s by %s\n", h.Target, h.Signer)
		}
	}
	printMetadata(w, d.Metadata)
	fmt.Fprintf(w, "content: %s", d.Content.State)
	if d.Content.Detail != "" {
		fmt.Fprintf(w, " (%s)", d.Content.Detail)
	}
	fmt.Fprintln(w)
}

// datasetCommands builds `create|ls|status` for a scope.
func datasetCommands(scope string) []*command {
	get := func(e *env, ctx context.Context) (node.ClusterNode, error) {
		if scope == node.ScopeLocal {
			return e.clusterLocal(ctx)
		}
		return e.clusterNet(ctx)
	}
	return []*command{
		{
			name:    "create",
			args:    "<name> [--replicas k] [--writer did]... [--note text] [--collection --facets facets.toml [--expire facet=400d] [--merge union|lww] [--ingest-hosts h,…] [--ingest-attempts n] [--ingest-timeout 2h] [--ingest-max-size 1T]]",
			summary: "create a dataset record (metadata only; content arrives with `add`/heads)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				replicas := fs.Int64("replicas", 0, "copies per blob (default 2; 1 in the local cluster)")
				note := fs.String("note", "", "free-form note")
				var writers []string
				fs.Func("writer", "did:key allowed to write heads (repeatable)", func(s string) error { writers = append(writers, s); return nil })
				coll := fs.Bool("collection", false, "a collection: items with metadata facets, member-selected holdings, union heads (replicas default 0)")
				facetFile := fs.String("facets", "", "facet declarations file (TOML or JSON): name = {ptr, type}")
				expire := fs.String("expire", "", "writers prune items whose time facet is older: <facet>=<duration>, e.g. sensed=400d")
				merge := fs.String("merge", "", "collection head merge: union (default) or lww")
				var ingest apiv0.DatasetIngest
				fs.Func("ingest-hosts", "host names item URLs may use in private mode (comma list, repeatable)", func(s string) error {
					for _, h := range strings.Split(s, ",") {
						if h = strings.TrimSpace(h); h != "" {
							ingest.Hosts = append(ingest.Hosts, h)
						}
					}
					return nil
				})
				fs.Int64Var(&ingest.Attempts, "ingest-attempts", 0, "fetch attempts per item URL file (default 6)")
				fs.StringVar(&ingest.Timeout, "ingest-timeout", "", "time limit per fetch attempt (default 2h)")
				ingestMax := fs.String("ingest-max-size", "", "size limit per item URL file (default 1T)")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("dataset create needs one name")
				}
				var facets map[string]apiv0.FacetDecl
				if *facetFile != "" {
					var err error
					if facets, err = readFacets(*facetFile); err != nil {
						return usagef("%v", err)
					}
				}
				c, err := get(e, ctx)
				if err != nil {
					return err
				}
				req := apiv0.DatasetCreateRequest{Name: fs.Arg(0), Replicas: *replicas, Writers: writers, Note: *note,
					Collection: *coll, Facets: facets, Expire: *expire, Merge: *merge}
				if *ingestMax != "" {
					var sz repo.Size
					if err := sz.UnmarshalTOML(*ingestMax); err != nil {
						return usagef("--ingest-max-size: %v", err)
					}
					ingest.MaxBytes = int64(sz)
				}
				if len(ingest.Hosts) > 0 || ingest.MaxBytes != 0 || ingest.Attempts != 0 || ingest.Timeout != "" {
					req.Ingest = &ingest
				}
				d, err := c.DatasetCreate(ctx, scope, req)
				if err != nil {
					return err
				}
				return e.emit(d, func(w io.Writer) { printDataset(w, d) })
			},
		},
		{
			name:    "ls",
			summary: "list datasets",
			run: func(ctx context.Context, e *env, args []string) error {
				if err := parseFlags(e.flags(), args); err != nil {
					return err
				}
				c, err := get(e, ctx)
				if err != nil {
					return err
				}
				ds, err := c.Datasets(ctx, scope)
				if err != nil {
					return err
				}
				return e.emit(ds, func(w io.Writer) {
					for _, d := range ds {
						head := "-"
						if d.Head != nil {
							head = d.Head.Target
						}
						fmt.Fprintf(w, "%s\treplicas %d\thead %s\tcontent %s\n", d.Name, d.Replicas, head, d.Content.State)
					}
				})
			},
		},
		{
			name:    "status",
			args:    "<name>",
			summary: "show a dataset's record, heads, metadata and content status",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("dataset status needs one name")
				}
				c, err := get(e, ctx)
				if err != nil {
					return err
				}
				d, err := c.Dataset(ctx, scope, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(d, func(w io.Writer) { printDataset(w, d) })
			},
		},
	}
}

var cmdDataset = &command{
	name:    "dataset",
	summary: "datasets of this node's local cluster (`" + nu.Name + " cluster dataset` for the networked one)",
	sub:     datasetCommands(node.ScopeLocal),
}

var cmdCluster = &command{
	name:    "cluster",
	summary: "networked cluster: membership, invites and signed shared state",
	sub: []*command{
		{
			name:    "init",
			args:    "<name> [--mode private|isolated|public]",
			summary: "found a cluster; this node gets the root and an admin key and becomes its first storage member",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				mode := fs.String("mode", "private", "network mode, fixed at genesis: private, isolated or public")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("cluster init needs a name")
				}
				c, err := e.clusterLocal(ctx)
				if err != nil {
					return err
				}
				info, err := c.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: fs.Arg(0), Mode: *mode})
				if err != nil {
					return err
				}
				return e.emit(info, func(w io.Writer) { printClusterInfo(w, "created", info) })
			},
		},
		{
			name:    "invite",
			args:    "[--role storage|client] [--tags zone=eu-1] [--capacity 8T] [--peer <peerId>] [--expires 24h]",
			summary: "print an invite token: pre-authorized with --peer (works offline), else a one-time secret",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				role := fs.String("role", "storage", "storage or client")
				tags := fs.String("tags", "", "comma-separated key=value tags")
				capacity := fs.String("capacity", "", "placement weight in bytes (K/M/G/T suffixes)")
				peer := fs.String("peer", "", "pre-authorize this PeerId (writes its member record now)")
				expires := fs.String("expires", "24h", "how long the token is valid")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 0 {
					return usagef("cluster invite takes only flags")
				}
				tm, err := parseTags(*tags)
				if err != nil {
					return usagef("%v", err)
				}
				cp, err := parseSize(*capacity)
				if err != nil {
					return usagef("%v", err)
				}
				c, err := e.clusterLocal(ctx)
				if err != nil {
					return err
				}
				inv, err := c.ClusterInvite(ctx, apiv0.ClusterInviteRequest{Role: *role, Tags: tm, Capacity: cp, Peer: *peer, Expires: *expires})
				if err != nil {
					return err
				}
				return e.emit(inv, func(w io.Writer) {
					fmt.Fprintln(w, inv.Token)
					fmt.Fprintf(e.stderr, "valid until %s; run `%s cluster join <token>` on the new node\n", inv.Expires.Local().Format(time.RFC3339), nu.Name)
					printMetadata(e.stderr, inv.Metadata)
				})
			},
		},
		{
			name:    "join",
			args:    "<token>",
			summary: "join a cluster with an invite token (needs networking; returns after the first state sync)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("cluster join needs one token")
				}
				c, err := e.clusterNet(ctx)
				if err != nil {
					return err
				}
				info, err := c.ClusterJoin(ctx, apiv0.ClusterJoinRequest{Token: fs.Arg(0)})
				if err != nil {
					return err
				}
				return e.emit(info, func(w io.Writer) { printClusterInfo(w, "joined", info) })
			},
		},
		{
			name:    "status",
			summary: "show the local and networked cluster: members, liveness, keys",
			run: func(ctx context.Context, e *env, args []string) error {
				if err := parseFlags(e.flags(), args); err != nil {
					return err
				}
				c, err := e.clusterLocal(ctx)
				if err != nil {
					return err
				}
				st, err := c.ClusterStatus(ctx)
				if err != nil {
					return err
				}
				return e.emit(st, func(w io.Writer) {
					if st.Network == nil {
						fmt.Fprintf(w, "not in a networked cluster (`%s cluster init` or `%s cluster join`)\n", nu.Name, nu.Name)
					} else {
						printClusterInfo(w, "network", st.Network)
					}
					if st.Local != nil {
						fmt.Fprintf(w, "local cluster: %d dataset(s)\n", st.Local.Datasets)
					}
				})
			},
		},
		{
			name:    "member",
			summary: "manage members",
			sub: []*command{{
				name:    "rm",
				args:    "<peerId>",
				summary: "remove a member: it is refused by every member once the state replicates",
				run: func(ctx context.Context, e *env, args []string) error {
					fs := e.flags()
					if err := parseFlags(fs, args); err != nil {
						return err
					}
					if fs.NArg() != 1 {
						return usagef("member rm needs one PeerId")
					}
					c, err := e.clusterLocal(ctx)
					if err != nil {
						return err
					}
					md, err := c.ClusterMemberRemove(ctx, apiv0.ClusterMemberRemoveRequest{Peer: fs.Arg(0)})
					if err != nil {
						return err
					}
					return e.emit(md, func(w io.Writer) {
						fmt.Fprintf(w, "removed %s\n", fs.Arg(0))
						printMetadata(w, *md)
					})
				},
			}},
		},
		{
			name:    "dataset",
			summary: "datasets of the networked cluster",
			sub:     datasetCommands(node.ScopeCluster),
		},
	},
}
