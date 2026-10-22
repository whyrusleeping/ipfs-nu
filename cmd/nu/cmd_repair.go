package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// Repair, drain, keys, audits and release commands (0048).

func repairNode(n any) (node.RepairNode, error) {
	r, ok := n.(node.RepairNode)
	if !ok {
		return nil, apiv0.NotImplemented("cluster administration")
	}
	return r, nil
}

func (e *env) repair(ctx context.Context) (node.RepairNode, error) {
	n, err := e.nodeOrLocalWrite(ctx)
	if err != nil {
		return nil, err
	}
	return repairNode(n)
}

func printAlerts(w io.Writer, indent string, alerts []apiv0.Alert) {
	for _, a := range alerts {
		who := a.Peer
		if a.Dataset != "" {
			who = a.Dataset
		}
		if who != "" {
			who = " " + who
		}
		fmt.Fprintf(w, "%sALERT %s%s: %s\n", indent, a.Code, who, a.Detail)
		if a.Action != "" {
			fmt.Fprintf(w, "%s  action: %s\n", indent, a.Action)
		}
	}
}

func printRelease(w io.Writer, r *apiv0.ReleaseReport) {
	mode := "applied"
	if r.DryRun {
		mode = "dry run"
	}
	fmt.Fprintf(w, "release %s (%s; release.auto %v): head %s\n", r.Dataset, mode, r.Auto, r.Target)
	for _, d := range r.Deferred {
		fmt.Fprintf(w, "  deferred: %s\n", d)
	}
	for _, c := range r.Candidates {
		verdict := "keep"
		if c.OK {
			verdict = "releasable"
		}
		fmt.Fprintf(w, "  %s %s: %d blob(s) %d B, %s", c.Kind, c.CID, c.Blobs, c.Bytes, verdict)
		if len(c.Confirmed) > 0 {
			fmt.Fprintf(w, ", confirmed by %s", strings.Join(c.Confirmed, ","))
		}
		if c.PendingUntil != nil {
			fmt.Fprintf(w, ", released after %s", c.PendingUntil.Local().Format(time.RFC3339))
		}
		fmt.Fprintln(w)
		for _, s := range c.Shortfall {
			fmt.Fprintf(w, "    shortfall %s: %s\n", s.Peer, s.Reason)
		}
	}
	if len(r.Candidates) == 0 {
		fmt.Fprintln(w, "  no release candidates")
	}
	if len(r.Released) > 0 || r.ReleasedBlobs > 0 {
		fmt.Fprintf(w, "released %d version(s), %d surplus blob(s) (%d B)\n", len(r.Released), r.ReleasedBlobs, r.ReleasedBytes)
	}
}

func printRemoval(w io.Writer, r *apiv0.MemberRemoval) {
	switch {
	case r.Removed && r.Forced:
		fmt.Fprintf(w, "FORCED removal of %s: the content below was left without k confirmed owner copies\n", r.Peer)
	case r.Removed:
		fmt.Fprintf(w, "removed %s\n", r.Peer)
	case r.Safe:
		fmt.Fprintf(w, "%s (%s) can be removed safely\n", r.Peer, r.State)
	default:
		fmt.Fprintf(w, "%s (%s) cannot be removed safely yet\n", r.Peer, r.State)
	}
	for _, s := range r.Reasons {
		fmt.Fprintf(w, "  %s\n", s)
	}
	for _, u := range r.Unconfirmed {
		fmt.Fprintf(w, "  unconfirmed %s %s: %d %s(s)", u.Dataset, u.Target, u.Units, u.Unit)
		if u.Detail != "" {
			fmt.Fprintf(w, " (%s)", u.Detail)
		}
		fmt.Fprintln(w)
		for _, m := range u.Missing {
			fmt.Fprintf(w, "    owner %s: %s\n", m.Peer, m.Reason)
		}
	}
	if r.Metadata != nil {
		printMetadata(w, *r.Metadata)
	}
}

func memberCommands() []*command {
	set := func(name, summary, state string) *command {
		usage := "<peerId>"
		if state == "" {
			usage += " [--state active|draining|unavailable] [--capacity 8T] [--store-limit 10T] [--tags zone=eu-1] [--label text]"
		}
		return &command{
			name:    name,
			args:    usage,
			summary: summary,
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				st := fs.String("state", state, "active, draining or unavailable")
				capacity := fs.String("capacity", "", "placement weight in bytes (K/M/G/T suffixes)")
				limit := fs.String("store-limit", "", "the member's physical budget (K/M/G/T suffixes)")
				tags := fs.String("tags", "", "comma-separated key=value tags (replaces the tags)")
				label := fs.String("label", "", "a label for operators")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("member %s needs one PeerId", name)
				}
				req := apiv0.MemberSetRequest{Peer: fs.Arg(0), State: *st, Label: *label}
				var err error
				if req.Capacity, err = parseSize(*capacity); err != nil {
					return usagef("%v", err)
				}
				if req.StoreLimit, err = parseSize(*limit); err != nil {
					return usagef("%v", err)
				}
				if req.Tags, err = parseTags(*tags); err != nil {
					return usagef("%v", err)
				}
				r, err := e.repair(ctx)
				if err != nil {
					return err
				}
				res, err := r.ClusterMemberSet(ctx, req)
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) {
					fmt.Fprintf(w, "%s %s %s\n", res.Member.Peer, strings.Join(res.Member.Roles, ","), res.Member.State)
					for _, warn := range res.Warnings {
						fmt.Fprintf(w, "  note: %s\n", warn)
					}
					printMetadata(w, res.Metadata)
				})
			},
		}
	}
	return []*command{
		{
			name:    "ls",
			summary: "list members with state, liveness and the repair authority",
			run: func(ctx context.Context, e *env, args []string) error {
				if err := parseFlags(e.flags(), args); err != nil {
					return err
				}
				c, err := e.clusterNet(ctx)
				if err != nil {
					return err
				}
				st, err := c.ClusterStatus(ctx)
				if err != nil {
					return err
				}
				if st.Network == nil {
					return apiv0.Errorf(apiv0.CodeNotFound, "this node is not in a networked cluster")
				}
				return e.emit(st.Network.Members, func(w io.Writer) { printClusterInfo(w, "network", st.Network) })
			},
		},
		set("set", "edit a member record (state, capacity, store limit, tags, label)", ""),
		set("drain", "drain a member: it owns nothing, keeps serving, and releases its copies as new owners confirm them", "draining"),
		{
			name:    "rm",
			args:    "<peerId> [--dry-run] [--force]",
			summary: "remove a member; refused while content would lack k confirmed copies without it (S6) unless --force (destructive)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				dry := fs.Bool("dry-run", false, "only report what lacks confirmation")
				force := fs.Bool("force", false, "remove even if content is left without k confirmed copies (destructive)")
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
				req := apiv0.ClusterMemberRemoveRequest{Peer: fs.Arg(0), DryRun: *dry, Force: *force}
				if *force && !*dry {
					// The destructive form prints the dry-run report first.
					if pre, err := c.ClusterMemberRemove(ctx, apiv0.ClusterMemberRemoveRequest{Peer: req.Peer, DryRun: true}); err == nil && !pre.Safe {
						printRemoval(e.stderr, pre)
					}
				}
				res, err := c.ClusterMemberRemove(ctx, req)
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) { printRemoval(w, res) })
			},
		},
	}
}

var cmdClusterKey = &command{
	name:    "key",
	summary: "authority keys: create one here, grant a role, revoke",
	sub: []*command{
		{
			name:    "new",
			summary: "create a P-256 authority key held by this node and print its did:key (an admin then grants it)",
			run: func(ctx context.Context, e *env, args []string) error {
				if err := parseFlags(e.flags(), args); err != nil {
					return err
				}
				r, err := e.repair(ctx)
				if err != nil {
					return err
				}
				res, err := r.ClusterKey(ctx, apiv0.KeyRequest{})
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) {
					fmt.Fprintln(w, res.DID)
					fmt.Fprintf(e.stderr, "grant it from an admin node: `%s cluster key grant %s admin|writer [--dataset <ds>]`\n", nu.Name, res.DID)
				})
			},
		},
		{
			name:    "grant",
			args:    "<did:key> admin|writer [--dataset <ds>]",
			summary: "authorize a key as admin, or as writer of one dataset",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				ds := fs.String("dataset", "", "the dataset a writer may write")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 2 {
					return usagef("key grant needs a did:key and a role")
				}
				r, err := e.repair(ctx)
				if err != nil {
					return err
				}
				res, err := r.ClusterKey(ctx, apiv0.KeyRequest{DID: fs.Arg(0), Role: fs.Arg(1), Dataset: *ds})
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) {
					fmt.Fprintf(w, "granted %s %s\n", res.Role, res.DID)
					if res.Metadata != nil {
						printMetadata(w, *res.Metadata)
					}
				})
			},
		},
		{
			name:    "revoke",
			args:    "<did:key>",
			summary: "revoke a key at its last commit held here (later commits by it are refused)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("key revoke needs a did:key")
				}
				r, err := e.repair(ctx)
				if err != nil {
					return err
				}
				res, err := r.ClusterKeyRevoke(ctx, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) {
					fmt.Fprintf(w, "revoked %s\n", res.DID)
					if res.Metadata != nil {
						printMetadata(w, *res.Metadata)
					}
				})
			},
		},
	},
}

var cmdClusterAudit = &command{
	name:    "audit",
	args:    "[--peer <peerId>] [--samples 1] [--scrub N | --full]",
	summary: "challenge storage members for verified chunk groups and scrub local content now (audits are not completion evidence)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		peer := fs.String("peer", "", "audit only this member")
		samples := fs.Int("samples", 1, "challenges per member")
		scrub := fs.Int("scrub", 0, "verify this many random local objects")
		full := fs.Bool("full", false, "verify every local object")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		r, err := e.repair(ctx)
		if err != nil {
			return err
		}
		res, err := r.ClusterAudit(ctx, apiv0.AuditRequest{Peer: *peer, Samples: *samples, Scrub: *scrub, Full: *full})
		if err != nil {
			return err
		}
		return e.emit(res, func(w io.Writer) {
			for _, c := range res.Challenges {
				v := "passed"
				if !c.OK {
					v = "FAILED: " + c.Error
				}
				fmt.Fprintf(w, "%s %s: group %d of %s %s in %dms\n", c.Peer, c.Dataset, c.Group, c.CID, v, c.MS)
			}
			if s := res.Scrub; s != nil {
				fmt.Fprintf(w, "scrub: %d object(s), %d B verified, %d corrupt (dropped and fetched again)\n", s.Checked, s.Bytes, len(s.Corrupt))
			}
			fmt.Fprintf(w, "note: %s\n", res.Note)
		})
	},
}

// releaseCommand is `dataset gc`: safe release on this member.
func releaseCommand(scope string) *command {
	return &command{
		name:    "gc",
		args:    "<name> [--dry-run]",
		summary: "release what this member no longer needs to keep (S1–S4): old versions and surplus blobs whose owners confirm them; --dry-run reports evidence and shortfall",
		run: func(ctx context.Context, e *env, args []string) error {
			fs := e.flags()
			dry := fs.Bool("dry-run", false, "only report candidates with their evidence or shortfall")
			if err := parseFlags(fs, args); err != nil {
				return err
			}
			if fs.NArg() != 1 {
				return usagef("dataset gc needs one name")
			}
			r, err := e.repair(ctx)
			if err != nil {
				return err
			}
			res, err := r.DatasetRelease(ctx, scope, fs.Arg(0), apiv0.ReleaseRequest{DryRun: *dry})
			if err != nil {
				return err
			}
			return e.emit(res, func(w io.Writer) { printRelease(w, res) })
		},
	}
}
