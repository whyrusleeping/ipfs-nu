package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// Version history commands (docs/cluster.md "Version history"; 0047).

// parseHistory parses --history keep_last=10,daily=30,…,max_bytes=1T (counts; all = -1).
func parseHistory(s string) (*apiv0.HistoryPolicy, error) {
	h := &apiv0.HistoryPolicy{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return nil, fmt.Errorf("want name=count pairs, got %q", kv)
		}
		var n int64
		var err error
		switch {
		case v == "all":
			n = -1
		case k == "max_bytes":
			n, err = parseSize(v)
		default:
			n, err = strconv.ParseInt(v, 10, 64)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", k, err)
		}
		dst := map[string]*int64{"keep_last": &h.KeepLast, "hourly": &h.Hourly, "daily": &h.Daily, "weekly": &h.Weekly,
			"monthly": &h.Monthly, "yearly": &h.Yearly, "max_bytes": &h.MaxBytes}[k]
		if dst == nil {
			return nil, fmt.Errorf("unknown schedule field %q (keep_last, hourly, daily, weekly, monthly, yearly, max_bytes)", k)
		}
		*dst = n
	}
	return h, nil
}

// importSource adds dir recursively (unpinned: the dataset's own pins keep it) and
// returns the tree CID.
func importSource(ctx context.Context, e *env, c node.ClusterNode, dir string) (string, error) {
	n, ok := c.(node.Node)
	if !ok {
		return "", apiv0.NotImplemented("adding content")
	}
	p, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", apiv0.Errorf(apiv0.CodeNotFound, "%s is not a readable directory", dir)
	}
	if !e.json {
		fmt.Fprintf(e.stderr, "%s: importing %s\n", nu.Name, p)
	}
	op, err := n.Add(ctx, apiv0.AddRequest{Path: p, Recursive: true})
	if err != nil {
		return "", err
	}
	final, err := n.WaitOperation(ctx, op.ID, 0)
	if err != nil {
		return "", err
	}
	if final.State != apiv0.StateSucceeded {
		if final.Error != nil {
			return "", final.Error
		}
		return "", apiv0.Errorf(apiv0.CodeFailed, "import %s", final.State)
	}
	var res apiv0.AddResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		return "", apiv0.Errorf(apiv0.CodeFailed, "malformed add result: %v", err)
	}
	return res.CID, nil
}

func history(c node.ClusterNode) (node.HistoryNode, error) {
	h, ok := c.(node.HistoryNode)
	if !ok {
		return nil, apiv0.NotImplemented("version history")
	}
	return h, nil
}

func printVersion(w io.Writer, v apiv0.VersionInfo) {
	mark := " "
	if v.Head {
		mark = "*"
	}
	label := ""
	if v.Label != "" {
		label = " [" + v.Label + "]"
	}
	live := "live"
	if !v.Live {
		live = "not live"
	}
	fmt.Fprintf(w, "%s %s %s %s%s %d file(s) %d B (+%d B new) %s, %s", mark, v.TID, v.Time.Local().Format(time.RFC3339), v.Root, label, v.Files, v.Bytes, v.NewBytes, live, v.Availability)
	if v.RestoredFrom != "" {
		fmt.Fprintf(w, ", restores %s", v.RestoredFrom)
	}
	if v.Note != "" {
		fmt.Fprintf(w, " — %s", v.Note)
	}
	fmt.Fprintln(w)
	if v.Detail != "" && v.Availability != apiv0.AvailComplete {
		fmt.Fprintf(w, "    %s\n", v.Detail)
	}
	for _, x := range v.Conflicts {
		fmt.Fprintf(w, "    conflict: %s redefines it as %s (ignored)\n", x.Signer, x.Root)
	}
}

func printHistoryPolicy(w io.Writer, h *apiv0.HistoryPolicy) {
	n := func(x int64) string {
		if x < 0 {
			return "all"
		}
		return strconv.FormatInt(x, 10)
	}
	fmt.Fprintf(w, "  history: keep_last %s, hourly %s, daily %s, weekly %s, monthly %s, yearly %s", n(h.KeepLast), n(h.Hourly), n(h.Daily), n(h.Weekly), n(h.Monthly), n(h.Yearly))
	if h.MaxBytes > 0 {
		fmt.Fprintf(w, ", max %d B", h.MaxBytes)
	}
	fmt.Fprintln(w)
}

// historyCommands builds `versions|diff|label|rollback|prune` for a scope.
func historyCommands(scope string, get func(*env, context.Context) (node.ClusterNode, error)) []*command {
	pre := nu.Name + " dataset"
	if scope == node.ScopeCluster {
		pre = nu.Name + " cluster dataset"
	}
	hget := func(e *env, ctx context.Context) (node.HistoryNode, error) {
		c, err := get(e, ctx)
		if err != nil {
			return nil, err
		}
		return history(c)
	}
	return []*command{
		{
			name:    "versions",
			args:    "<name>",
			summary: "list an archive's versions with retention reasons and availability (apart from readiness)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("dataset versions needs one name")
				}
				h, err := hget(e, ctx)
				if err != nil {
					return err
				}
				vs, err := h.DatasetVersions(ctx, scope, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(vs, func(w io.Writer) {
					fmt.Fprintf(w, "%s: %d version(s), live set %d B\n", vs.Name, len(vs.Versions), vs.LiveBytes)
					if vs.History != nil {
						printHistoryPolicy(w, vs.History)
					}
					if vs.Excess > 0 {
						fmt.Fprintf(w, "  warning: protected versions exceed max_bytes by %d B\n", vs.Excess)
					}
					if a := vs.Archive; a != nil {
						fmt.Fprintf(w, "  archive-wide: %s, %s\n", a.State, copiesText(*a))
					}
					for _, v := range vs.Versions {
						printVersion(w, v)
					}
				})
			},
		},
		{
			name:    "diff",
			args:    "<name> <from> <to>",
			summary: "paths added, removed and changed between two versions (TID, label or time)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 3 {
					return usagef("dataset diff needs a name and two versions")
				}
				h, err := hget(e, ctx)
				if err != nil {
					return err
				}
				d, err := h.DatasetDiff(ctx, scope, fs.Arg(0), fs.Arg(1), fs.Arg(2))
				if err != nil {
					return err
				}
				return e.emit(d, func(w io.Writer) {
					for _, x := range [][2]any{{"+", d.Added}, {"-", d.Removed}, {"~", d.Changed}} {
						for _, p := range x[1].([]string) {
							fmt.Fprintf(w, "%s %s\n", x[0], p)
						}
					}
					if d.Truncated {
						fmt.Fprintln(w, "… (truncated)")
					}
				})
			},
		},
		{
			name:    "label",
			args:    "<name> <version> [<label>] [--rm] [--note text] [--reacquire]",
			summary: "label a version, keeping its content (a label on missing content needs --reacquire)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				rm := fs.Bool("rm", false, "remove the version's label")
				note := fs.String("note", "", "replace the version's note")
				reacq := fs.Bool("reacquire", false, "make a version whose content is missing an acquisition obligation again")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 3 && !(*rm && fs.NArg() == 2) {
					return usagef("dataset label needs a name, a version and a label (or --rm)")
				}
				h, err := hget(e, ctx)
				if err != nil {
					return err
				}
				l, err := h.DatasetLabel(ctx, scope, fs.Arg(0), apiv0.VersionLabelRequest{Version: fs.Arg(1), Label: fs.Arg(2), Remove: *rm, Note: *note, Reacquire: *reacq})
				if err != nil {
					return err
				}
				return e.emit(l, func(w io.Writer) {
					printVersion(w, l.Version)
					printMetadata(w, l.Metadata)
				})
			},
		},
		{
			name:    "rollback",
			args:    "<name> <version> [--note text]",
			summary: "write a new head restoring a retained-complete version (history is never rewritten)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				note := fs.String("note", "", "note for the new version (default: rollback to <tid>)")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 2 {
					return usagef("dataset rollback needs a name and a version")
				}
				h, err := hget(e, ctx)
				if err != nil {
					return err
				}
				d, err := h.DatasetRollback(ctx, scope, fs.Arg(0), apiv0.RollbackRequest{Version: fs.Arg(1), Note: *note})
				if err != nil {
					return err
				}
				return e.emit(d, func(w io.Writer) { printDataset(w, d) })
			},
		},
		{
			name:    "prune",
			args:    "<name> [--dry-run]",
			summary: "release versions outside the live set (bytes freed and shared are reported; " + pre + " prune --dry-run first)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				dry := fs.Bool("dry-run", false, "only report the impact")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("dataset prune needs one name")
				}
				h, err := hget(e, ctx)
				if err != nil {
					return err
				}
				r, err := h.DatasetPrune(ctx, scope, fs.Arg(0), apiv0.PruneRequest{DryRun: *dry})
				if err != nil {
					return err
				}
				return e.emit(r, func(w io.Writer) {
					verb := "would release"
					if !r.DryRun {
						verb = "released"
					}
					fmt.Fprintf(w, "%s %d version(s): %d B freed, %d B shared with other pins (kept)\n", verb, len(r.Candidates), r.FreeBytes, r.SharedKept)
					for _, c := range r.Candidates {
						fmt.Fprintf(w, "  %s %s %s\n", c.TID, c.Root, c.Label)
					}
					for _, warn := range r.Warnings {
						fmt.Fprintf(w, "warning: %s\n", warn)
					}
				})
			},
		},
	}
}
