package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/target"
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
	reps := strconv.FormatInt(d.Replicas, 10)
	if d.All {
		reps = "all"
	}
	fmt.Fprintf(w, "%s (%s cluster) replicas %s, record by %s\n", d.Name, d.Cluster, reps, d.Signer)
	if d.Spread != "" || len(d.Where) > 0 {
		fmt.Fprintf(w, "  placement: spread %q, where %v\n", d.Spread, d.Where)
	}
	for _, warn := range d.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
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
		fmt.Fprintf(w, "  head %s (%s) by %s", d.Head.Target, d.Head.Kind, d.Head.Signer)
		if d.Head.Rev != "" {
			fmt.Fprintf(w, ", rev %s", d.Head.Rev)
		}
		if d.Head.Status != "" {
			fmt.Fprintf(w, ", account %s", d.Head.Status)
		}
		fmt.Fprintln(w)
	}
	if len(d.Heads) > 1 {
		for _, h := range d.Heads {
			fmt.Fprintf(w, "  writer head %s by %s\n", h.Target, h.Signer)
		}
	}
	if d.History != nil {
		printHistoryPolicy(w, d.History)
	}
	if d.SourcePath != "" {
		fmt.Fprintf(w, "  source: %s\n", d.SourcePath)
	}
	if a := d.Atrepo; a != nil {
		fmt.Fprintf(w, "  source: at://%s (%s), account %s, redistribution %s", a.DID, a.Handle, a.Status, a.Redistribution)
		if len(a.NSIDs) > 0 {
			fmt.Fprintf(w, ", nsids %s", strings.Join(a.NSIDs, ","))
		}
		if a.Blobs {
			fmt.Fprintf(w, ", blobs (%d fetched, %d oversized skipped, %d failed)", a.BlobsFetched, a.BlobsOversized, a.BlobsFailed)
		}
		fmt.Fprintln(w)
		if a.PDSRev != "" && a.PDSRev != a.Rev {
			fmt.Fprintf(w, "    lag: held rev %s, PDS rev %s\n", a.Rev, a.PDSRev)
		}
		if !a.Refreshed.IsZero() {
			fmt.Fprintf(w, "    last update %s (%s)", a.Refreshed.Local().Format(time.RFC3339), a.Mode)
			if a.Error != "" {
				fmt.Fprintf(w, ": %s", a.Error)
			}
			fmt.Fprintln(w)
		}
	}
	if d.Version != nil {
		printVersion(w, *d.Version)
	}
	printMetadata(w, d.Metadata)
	printContent(w, d.Content)
	if a := d.Archive; a != nil {
		fmt.Fprintf(w, "archive-wide (live set %.16s): %s, %s\n", a.Live, a.State, copiesText(*a))
	}
}

// copiesText summarises confirmed copies per unit (E6/E7).
func copiesText(c apiv0.ContentState) string {
	s := fmt.Sprintf("%d confirmed cop(ies) required per unit, %d member(s) confirming", c.Required, len(c.Confirmed))
	if p := c.Copies; p != nil {
		s = fmt.Sprintf("%d confirmed cop(ies) required per %s: %d of %d %[2]s(s) below, fewest %d; %d member(s) confirming",
			c.Required, p.Unit, p.Below, p.Units, p.Min, len(c.Confirmed))
	}
	return s
}

// printContent prints a content status with its W3 fields.
func printContent(w io.Writer, c apiv0.ContentState) {
	fmt.Fprintf(w, "content: %s", c.State)
	if c.Detail != "" {
		fmt.Fprintf(w, " (%s)", c.Detail)
	}
	fmt.Fprintln(w)
	if c.Target == "" {
		return
	}
	fmt.Fprintf(w, "  target %s, %s", c.Target, copiesText(c))
	if len(c.Confirmed) > 0 {
		fmt.Fprintf(w, ", oldest evidence %s", (time.Duration(c.EvidenceAge) * time.Millisecond).Round(time.Millisecond))
	}
	fmt.Fprintln(w)
	if c.Epoch != "" {
		fmt.Fprintf(w, "  epoch %.16s, policy %.16s, mode %s\n", c.Epoch, c.Policy, c.Mode)
	}
	for _, m := range c.Missing {
		fmt.Fprintf(w, "  missing %s: %s\n", m.Peer, m.Reason)
	}
	for _, x := range c.Extra {
		fmt.Fprintf(w, "  extra %s (not counted)\n", x.Peer)
	}
	for _, u := range c.Unmet {
		fmt.Fprintf(w, "  unmet: %s\n", u)
	}
	if lr := c.LastReady; lr != nil && c.State != "ready" {
		fmt.Fprintf(w, "  last ready at %s (epoch %.16s)\n", lr.At.Local().Format(time.RFC3339), lr.Epoch)
	}
	if l := c.Local; l != nil {
		unit := "blob"
		if c.Copies != nil {
			unit = c.Copies.Unit
		}
		fmt.Fprintf(w, "  this node: %s %s (%s), owns %d %s(s) %d B, missing %d %s(s) %d B", l.State, l.Target, l.Mode, l.Owned.Blobs, unit, l.Owned.Bytes, l.Missing.Blobs, unit, l.Missing.Bytes)
		if l.Observed != "" {
			fmt.Fprintf(w, "; head %s observed only (staging full)", l.Observed)
		}
		if l.Reason != "" {
			fmt.Fprintf(w, "; %s", l.Reason)
		}
		if l.Need > 0 {
			fmt.Fprintf(w, ", %d B more needed", l.Need)
		}
		fmt.Fprintln(w)
	}
	if r := c.Release; r != nil && len(r.Candidates) > 0 {
		fmt.Fprintf(w, "  %d retained version(s) are release candidates; release.auto is off, nothing is released\n", len(r.Candidates))
	}
}

// datasetCommands builds `create|ls|status` for a scope.
func datasetCommands(scope string) []*command {
	get := func(e *env, ctx context.Context) (node.ClusterNode, error) {
		if scope == node.ScopeLocal {
			return e.clusterLocal(ctx)
		}
		return e.clusterNet(ctx)
	}
	return append([]*command{
		{
			name:    "create",
			args:    "<name> [--replicas k] [--writer did]... [--note text] [--collection --facets facets.toml [--expire facet=400d] [--merge union|lww] [--ingest-hosts h,…] [--ingest-attempts n] [--ingest-timeout 2h] [--ingest-max-size 1T]]",
			summary: "create a dataset record (metadata only; content arrives with `add`/heads)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				replicasFlag := fs.String("replicas", "", "copies per blob, or all (default 2; 1 in the local cluster)")
				spread := fs.String("spread", "", "tag whose values each blob's owners must differ in, e.g. zone (team default: zone when every member is zone-tagged)")
				var where map[string][]string
				fs.Func("where", "placement tag filter tag=v1|v2 (repeatable; every tag must match)", func(s string) error {
					k, v, ok := strings.Cut(s, "=")
					if !ok || k == "" || v == "" {
						return fmt.Errorf("want tag=value[|value…]")
					}
					if where == nil {
						where = map[string][]string{}
					}
					where[k] = append(where[k], strings.Split(v, "|")...)
					return nil
				})
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
				archive := fs.Bool("archive", false, "archive preset: keep versions under the default retention schedule (see --history)")
				mirror := fs.Bool("mirror", false, "mirror preset: follow current state with the protected-version safeguard")
				hist := fs.String("history", "", "retention schedule, e.g. keep_last=10,daily=30,monthly=12,yearly=all[,max_bytes=1T] (implies --archive)")
				source := fs.String("source", "", "directory `"+nu.Name+" dataset update <name>` re-adds (imported now as the first version), or at://<handle-or-did>: an atproto account archived from its PDS (0054)")
				nsids := fs.String("nsids", "", "atproto source: comma list of record collections that count as the dataset (default all)")
				blobs := fs.Bool("blobs", false, "atproto source: also fetch the records' blobs (over 8 MiB are skipped)")
				publishHistory := fs.Bool("publish-history", false, "atproto source: let retained history of a third-party account leave the cluster (recorded on the dataset; private by default)")
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
				req := apiv0.DatasetCreateRequest{Name: fs.Arg(0), Writers: writers, Note: *note,
					Collection: *coll, Facets: facets, Expire: *expire, Merge: *merge, Where: where, Spread: *spread}
				switch *replicasFlag {
				case "":
				case "all":
					req.All = true
				default:
					k, err := strconv.ParseInt(*replicasFlag, 10, 64)
					if err != nil || k < 0 {
						return usagef("--replicas must be a number or all")
					}
					req.Replicas = k
				}
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
				req.Archive, req.Mirror = *archive, *mirror
				if *hist != "" {
					if req.History, err = parseHistory(*hist); err != nil {
						return usagef("--history: %v", err)
					}
				}
				if strings.HasPrefix(*source, "at://") {
					req.SourceAt, req.Blobs, req.PublishHistory = *source, *blobs, *publishHistory
					for _, id := range strings.Split(*nsids, ",") {
						if id = strings.TrimSpace(id); id != "" {
							req.NSIDs = append(req.NSIDs, id)
						}
					}
				} else if *source != "" {
					if req.SourcePath, err = filepath.Abs(*source); err != nil {
						return err
					}
				}
				d, err := c.DatasetCreate(ctx, scope, req)
				if err != nil {
					return err
				}
				if req.SourcePath != "" { // the first version
					root, err := importSource(ctx, e, c, req.SourcePath)
					if err != nil {
						return err
					}
					if d, err = c.DatasetUpdate(ctx, scope, d.Name, apiv0.DatasetUpdateRequest{Target: root}); err != nil {
						return err
					}
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
			name:    "update",
			args:    "<name> [<cid> | <dir>] [--kind tree|file|blob] [--note text]",
			summary: "write a dataset's head (a CID, or a directory imported first; without one the dataset's --source); metadata and content are reported separately",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				kind := fs.String("kind", "", "head kind (default: detected from held content)")
				note := fs.String("note", "", "free-form note")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 && fs.NArg() != 2 {
					return usagef("dataset update needs a name and a target CID or directory")
				}
				c, err := get(e, ctx)
				if err != nil {
					return err
				}
				tgt := fs.Arg(1)
				if tgt == "" { // re-add the recorded source
					cur, err := c.Dataset(ctx, scope, fs.Arg(0))
					if err != nil {
						return err
					}
					if cur.Atrepo != nil { // fetch the account's current commit now
						d, err := c.DatasetUpdate(ctx, scope, fs.Arg(0), apiv0.DatasetUpdateRequest{Note: *note})
						if err != nil {
							return err
						}
						return e.emit(d, func(w io.Writer) { printDataset(w, d) })
					}
					if cur.SourcePath == "" {
						return usagef("dataset %s has no --source directory; give a CID or a directory", fs.Arg(0))
					}
					tgt = cur.SourcePath
				}
				if !target.IsCID(tgt) {
					if tgt, err = importSource(ctx, e, c, tgt); err != nil {
						return err
					}
				}
				d, err := c.DatasetUpdate(ctx, scope, fs.Arg(0), apiv0.DatasetUpdateRequest{Target: tgt, Kind: *kind, Note: *note})
				if err != nil {
					return err
				}
				return e.emit(d, func(w io.Writer) { printDataset(w, d) })
			},
		},
		{
			name:    "wait",
			args:    "<name> <cid> [--condition ready|held|metadata|archive-ready] [--holder peer]... [--acks n] [--timeout 10m]",
			summary: "wait for one condition on an exact target (exit 6 on timeout); the result names what is still missing",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				cond := fs.String("condition", "ready", "metadata (signed state held by --acks storage members), held (every --holder has it complete) or ready (policy readiness)")
				var holders []string
				fs.Func("holder", "PeerId that must hold the target (held; repeatable)", func(s string) error { holders = append(holders, s); return nil })
				acks := fs.Int("acks", 1, "storage members that must hold the signed state (metadata)")
				mode := fs.String("mode", "", "report mode a holder must use (held; default any)")
				timeout := fs.String("timeout", "", "give up after this long (default 30s for metadata, 10m otherwise; 0 waits indefinitely)")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 2 {
					return usagef("dataset wait needs a name and a target CID")
				}
				c, err := get(e, ctx)
				if err != nil {
					return err
				}
				res, err := c.DatasetWait(ctx, scope, fs.Arg(0), apiv0.DatasetWaitRequest{Target: fs.Arg(1), Condition: *cond, Holders: holders, Acks: *acks, Mode: *mode, Timeout: *timeout})
				if res != nil {
					if eerr := e.emit(res, func(w io.Writer) { printWait(w, res) }); eerr != nil {
						return eerr
					}
				}
				return err
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
	}, historyCommands(scope, get)...)
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

func printWait(w io.Writer, r *apiv0.WaitResult) {
	verdict := "satisfied"
	if !r.Satisfied {
		verdict = "NOT satisfied"
	}
	fmt.Fprintf(w, "%s: %s\n", r.Condition, verdict)
	if r.SupersededBy != "" {
		fmt.Fprintf(w, "  the head moved to %s during the wait; the wait stayed on %s\n", r.SupersededBy, r.Target)
	}
	if m := r.Metadata; m != nil {
		fmt.Fprintf(w, "  metadata: rev %s by %s held by %d of %d storage member(s)\n", m.Rev, m.Signer, len(m.AckedBy), m.Acks)
	}
	printContent(w, r.ContentState)
}
