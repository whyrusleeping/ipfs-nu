package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/target"
)

// atproto records, repository snapshots and CAR import/export (0053).

func atprotoNode(n node.Node) (node.AtprotoNode, error) {
	a, ok := n.(node.AtprotoNode)
	if !ok {
		return nil, apiv0.NotImplemented("atproto and CAR commands")
	}
	return a, nil
}

// getAt is `get` for at:// targets: a record prints (or writes with -o) the verified
// record envelope as JSON; a repository is snapshotted into the store and summarised.
func getAt(ctx context.Context, e *env, raw, out string, blobs, noWait bool) error {
	u, err := atproto.ParseURI(raw)
	if err != nil {
		return err
	}
	n, err := e.nodeOrLocal(ctx)
	if err != nil {
		return err
	}
	a, err := atprotoNode(n)
	if err != nil {
		return err
	}
	if u.IsRecord() {
		rec, err := a.AtRecord(ctx, raw)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if out != "" {
			if err := os.WriteFile(out, b, 0o644); err != nil {
				return apiv0.Errorf(apiv0.CodeFailed, "cannot write %s: %v", out, err)
			}
			if !e.json {
				fmt.Fprintf(e.stdout, "wrote %s (%s at rev %s) to %s\n", rec.URI, rec.CID, rec.Rev, out)
				return nil
			}
		}
		_, err = e.stdout.Write(b)
		return err
	}
	if u.Collection != "" {
		return usagef("get takes a record (at://<did>/<collection>/<rkey>) or a repository (at://<did>); list a collection with `%s ls %s`", nu.Name, raw)
	}
	if out != "" {
		return usagef("a repository snapshot is stored and pinned, not written to a file; export it with `%s car export <commit> --profile atrepo`", nu.Name)
	}
	op, err := a.AtSnapshot(ctx, apiv0.AtSnapshotRequest{Target: raw, Blobs: blobs})
	if err != nil {
		return err
	}
	return finishOperation(e, ctx, n, op, noWait, func(w io.Writer, res apiv0.AtSnapshot) {
		fmt.Fprintf(w, "snapshot of %s at rev %s: commit %s (%d records, %d MST nodes, %s), pinned under %s\n",
			res.DID, res.Rev, res.Commit, res.Records, res.Nodes, humanBytes(res.Bytes), res.Profile)
		if res.Profile == "atrepo+blobs" {
			fmt.Fprintf(w, "blobs: %d fetched, %d oversized (> 8 MiB, skipped), %d failed\n", res.Blobs, res.BlobsOversized, res.BlobsFailed)
		}
		for _, nc := range res.NonConforming {
			fmt.Fprintf(w, "non-conforming record kept: %s\n", nc)
		}
		fmt.Fprintf(w, "browse: %s ls at://%s\n", nu.Name, res.DID)
	})
}

var cmdCar = &command{
	name:    "car",
	summary: "import and export DASL CARs under a walk profile",
	sub: []*command{
		{
			name:    "import",
			args:    "[--profile nu-tree|atrepo|drisl] [--key <did:key>] [--offline] [--no-pin] [--no-wait] <file.car>",
			summary: "verify a CAR (structure, and an atproto repo's signature when the identity or a key is available), store and pin it",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				req := apiv0.CarImportRequest{Pin: true}
				fs.StringVar(&req.Profile, "profile", "", "walk profile: nu-tree, atrepo or drisl (default: detected from the root block)")
				fs.StringVar(&req.Key, "key", "", "signing key (did:key or publicKeyMultibase) an atrepo commit must verify against")
				fs.BoolVar(&req.Offline, "offline", false, "do not resolve the repository's DID to check its signature")
				noPin := fs.Bool("no-pin", false, "store without pinning")
				noWait := fs.Bool("no-wait", false, "print the operation and return instead of following it")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("car import needs exactly one file")
				}
				req.Pin = !*noPin
				p, err := filepath.Abs(fs.Arg(0))
				if err != nil {
					return err
				}
				if _, err := os.Stat(p); err != nil {
					return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", fs.Arg(0), err).WithAction("check the path")
				}
				req.Path = p
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				a, err := atprotoNode(n)
				if err != nil {
					return err
				}
				op, err := a.CarImport(ctx, req)
				if err != nil {
					return err
				}
				return finishOperation(e, ctx, n, op, *noWait, func(w io.Writer, res apiv0.CarImport) {
					fmt.Fprintf(w, "imported %s (%s profile, %d blocks, %s", res.Root, res.Profile, res.Blocks, humanBytes(res.Bytes))
					if res.Unreferenced > 0 {
						fmt.Fprintf(w, ", %d unreferenced", res.Unreferenced)
					}
					fmt.Fprintln(w, ")")
					if res.DID != "" {
						fmt.Fprintf(w, "atproto repository of %s at rev %s: %d records; signature %s", res.DID, res.Rev, res.Records, res.Signature)
						if res.SignedBy != "" {
							fmt.Fprintf(w, " (%s)", res.SignedBy)
						}
						fmt.Fprintln(w)
						for _, nc := range res.NonConforming {
							fmt.Fprintf(w, "non-conforming record kept: %s\n", nc)
						}
					}
					if res.Pinned {
						fmt.Fprintf(w, "pinned %s\n", res.Root)
					}
				})
			},
		},
		{
			name:    "export",
			args:    "[--profile nu-tree|atrepo|drisl] [-o file.car] <cid>",
			summary: "write a held root as a CAR (atrepo in streamable order; stdout by default)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				profile := fs.String("profile", "", "walk profile: nu-tree, atrepo or drisl (default: detected from the root block)")
				out := fs.String("o", "", "output file (default: stdout)")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || !target.IsCID(fs.Arg(0)) {
					return usagef("car export needs one CID")
				}
				n, err := e.nodeOrLocal(ctx)
				if err != nil {
					return err
				}
				a, err := atprotoNode(n)
				if err != nil {
					return err
				}
				rc, err := a.CarExport(ctx, fs.Arg(0), *profile)
				if err != nil {
					return err
				}
				defer rc.Close()
				var w io.Writer = e.stdout
				if *out != "" {
					f, err := os.Create(*out)
					if err != nil {
						return apiv0.Errorf(apiv0.CodeFailed, "cannot write %s: %v", *out, err)
					}
					defer f.Close()
					w = f
				}
				nb, err := io.Copy(w, rc)
				if err != nil {
					return apiv0.Errorf(apiv0.CodeFailed, "export failed after %s: %v", humanBytes(nb), err)
				}
				if *out != "" && !e.json {
					fmt.Fprintf(e.stderr, "%s: wrote %s to %s\n", nu.Name, humanBytes(nb), *out)
				}
				return nil
			},
		},
	},
}
