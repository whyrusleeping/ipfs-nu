package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/store"
)

// 0044: offline repo maintenance (docs/operations.md "Backup, export and recovery").

// offline returns the repo with no daemon running; the daemon would change the store and
// the cluster state while a snapshot is taken.
func (e *env) offline(ctx context.Context) (*repo.Repo, error) {
	r, err := e.openRepo()
	if err != nil {
		return nil, err
	}
	if c, err := client.Discover(r); err == nil {
		if _, err := c.Status(ctx); err == nil {
			return nil, apiv0.Errorf(apiv0.CodeConflict, "a daemon is running on %s", r.Root()).
				WithAction("stop it first: backups, exports and restores work on a stopped node so the snapshot is consistent")
		}
	}
	return r, nil
}

// offlineNode opens the node in-process under the repo lock, without networking.
func (e *env) offlineNode(ctx context.Context) (*node.Local, error) {
	r, err := e.offline(ctx)
	if err != nil {
		return nil, err
	}
	n, release, err := e.local(r, false)
	if err != nil {
		return nil, err
	}
	e.cleanup = append(e.cleanup, release)
	return n, nil
}

func absArg(fs interface{ Arg(int) string }) (string, error) { return filepath.Abs(fs.Arg(0)) }

var cmdRepo = &command{
	name:    "repo",
	summary: "back up, export, import and restore $" + repo.EnvPath + " (on a stopped node)",
	sub: []*command{
		{
			name:    "export",
			args:    "<dir>",
			summary: "write every pin and its held content as a verified bundle (content.car, blobs/, manifest.json) into a new or empty directory",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("repo export needs a directory")
				}
				dir, err := absArg(fs)
				if err != nil {
					return err
				}
				n, err := e.offlineNode(ctx)
				if err != nil {
					return err
				}
				m, err := n.ExportContent(ctx, dir)
				if err != nil {
					return err
				}
				return e.emit(m, func(w io.Writer) {
					fmt.Fprintf(w, "exported %d pins (%d blocks, %d large blobs) to %s\n", len(m.Pins), m.CAR.Blocks, len(m.Blobs), dir)
					if !m.Complete {
						fmt.Fprintln(w, "warning: some pins were not completely held; the bundle is not a complete export:")
						for _, p := range m.Pins {
							if p.Missing > 0 {
								fmt.Fprintf(w, "  %s (%s %s): %d of %d objects missing\n", p.CID, p.Source, p.Name, p.Missing, p.Objects)
							}
						}
					}
					fmt.Fprintf(w, "keys and cluster state are not in it: back them up with `%s repo backup`\n", nu.Name)
				})
			},
		},
		{
			name:    "import",
			args:    "<dir>",
			summary: "verify a bundle written by `repo export` into the store and restore its pins",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("repo import needs a directory")
				}
				dir, err := absArg(fs)
				if err != nil {
					return err
				}
				n, err := e.offlineNode(ctx)
				if err != nil {
					return err
				}
				res, err := n.ImportContent(ctx, dir)
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) {
					fmt.Fprintf(w, "imported %d blocks and %d large blobs (%d bytes), restored %d pins\n", res.Blocks, res.Blobs, res.Bytes, res.Pins)
					for _, p := range res.Incomplete {
						fmt.Fprintf(w, "incomplete: %s (%s %s): %d of %d objects missing; fetch them from a provider\n", p.CID, p.Source, p.Name, p.Missing, p.Objects)
					}
				})
			},
		},
		{
			name:    "backup",
			args:    "<file>",
			summary: "write the node key, config, push tokens, cluster keys and signed cluster state to a new 0600 tar file (plaintext keys: keep it safe)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("repo backup needs a file")
				}
				p, err := absArg(fs)
				if err != nil {
					return err
				}
				n, err := e.offlineNode(ctx)
				if err != nil {
					return err
				}
				f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, repo.FileMode)
				if errors.Is(err, os.ErrExist) {
					return apiv0.Errorf(apiv0.CodeConflict, "%s exists", p).WithAction("choose a new file name")
				}
				if err != nil {
					return err
				}
				m, err := n.BackupAuthority(f)
				if err == nil {
					err = f.Sync()
				}
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					os.Remove(p)
					return err
				}
				return e.emit(m, func(w io.Writer) {
					fmt.Fprintf(w, "backed up %d files of peer %s to %s\n", len(m.Files), m.PeerID, p)
					for _, c := range m.Clusters {
						fmt.Fprintf(w, "  %s cluster %q (%s): %d keys, %d state repositories\n", c.Kind, c.Name, c.ID, len(c.Keys), len(c.Heads))
					}
					fmt.Fprintln(w, "the file holds private keys unencrypted: store it offline or encrypt it yourself")
				})
			},
		},
		{
			name:    "restore",
			args:    "<file>",
			summary: "restore a `repo backup` into a fresh $" + repo.EnvPath + " and verify its signed cluster state",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("repo restore needs a file")
				}
				p, err := absArg(fs)
				if err != nil {
					return err
				}
				f, err := os.Open(p)
				if err != nil {
					return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", p, err).WithAction("check the path")
				}
				defer f.Close()
				r, err := e.offline(ctx)
				if err != nil {
					return err
				}
				lock, err := r.Lock()
				if err != nil {
					return apiv0.Errorf(apiv0.CodeConflict, "%v", err).WithAction("stop the daemon first")
				}
				defer lock.Release()
				m, err := node.RestoreAuthority(r, f)
				if err != nil {
					return err
				}
				return e.emit(m, func(w io.Writer) {
					fmt.Fprintf(w, "restored peer %s into %s (%d files, %d state repositories verified)\n", m.PeerID, r.Root(), len(m.Files), m.Verified)
					fmt.Fprintf(w, "next: `%s repo import <dir>` for an exported bundle, or start the daemon to re-acquire content from other members\n", nu.Name)
				})
			},
		},
		{
			name:    "unpin",
			args:    "--source <source> [--name <name>] <cid>",
			summary: "remove one pin of any source (orphaned pins after an import or a re-genesis); subsystems re-pin what they still need",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				source := fs.String("source", "", "pin source as shown by `pin ls` (explicit, cluster, follow, …)")
				name := fs.String("name", "", "pin name as shown by `pin ls`")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || *source == "" {
					return usagef("repo unpin needs --source and one CID")
				}
				c, err := cid.Parse(fs.Arg(0))
				if err != nil {
					return usagef("repo unpin: %v", err)
				}
				n, err := e.offlineNode(ctx)
				if err != nil {
					return err
				}
				removed := false
				for _, mode := range []string{store.Recursive, store.Direct} {
					ok, err := n.Store.Unpin(store.Pin{CID: c, Mode: mode, Source: *source, Name: *name})
					if err != nil {
						return err
					}
					removed = removed || ok
				}
				if !removed {
					return apiv0.Errorf(apiv0.CodeNotFound, "no %s pin %q of %s", *source, *name, c).WithAction("list pins with `" + nu.Name + " pin ls`")
				}
				return e.emit(apiv0.Pin{CID: c.String()}, func(w io.Writer) { fmt.Fprintf(w, "unpinned %s (%s %s)\n", c, *source, *name) })
			},
		},
	},
}
