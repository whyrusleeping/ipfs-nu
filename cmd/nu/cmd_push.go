package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/token"
)

// Item pushes (docs/collections.md "Ingest"): `nu cluster collection put|rm|pushes` and
// the push tokens of `nu api token`.

func init() {
	cmdCollection.sub = append(cmdCollection.sub, cmdCollectionPut, cmdCollectionRm, cmdCollectionPushes)
	root.sub = append(root.sub, cmdAPI)
}

func (e *env) pushes(ctx context.Context) (node.PushNode, error) {
	n, err := e.nodeOrLocal(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := n.(node.PushNode)
	if !ok {
		return nil, apiv0.NotImplemented("item pushes")
	}
	return p, nil
}

// remoteAPI reports whether --api names another machine, so local files must be
// uploaded rather than passed as paths.
func (e *env) remoteAPI() bool {
	if e.apiAddr == "" {
		return false
	}
	a := e.apiAddr
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	host, _, err := net.SplitHostPort(strings.TrimRight(a, "/"))
	if err != nil {
		host = a
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return !(host == "localhost" || ip != nil && ip.IsLoopback())
}

// pushFiles expands file arguments ([key=]path; a directory adds every regular file
// below it, keyed by its relative path under key).
func pushFiles(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		key, p, ok := strings.Cut(a, "=")
		if !ok {
			key, p = "", a
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", p, err)
		}
		if !fi.IsDir() {
			if key == "" {
				key = filepath.Base(abs)
			}
			out[key] = abs
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(abs, path)
			k := filepath.ToSlash(rel)
			if key != "" {
				k = key + "/" + k
			}
			out[k] = path
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func printPush(w io.Writer, p *apiv0.CollectionPush) {
	fmt.Fprintf(w, "%s: %s (push %s", p.Path, p.State, p.ID)
	if p.Duplicate {
		fmt.Fprint(w, ", already pushed")
	}
	fmt.Fprintln(w, ")")
	if p.Entry != "" {
		fmt.Fprintf(w, "  entry %s (%d B), meta %s\n", p.Entry, p.Size, p.Meta)
	}
	if p.Head != "" {
		fmt.Fprintf(w, "  in writer head %s\n", p.Head)
	}
	if p.Error != nil {
		fmt.Fprintf(w, "  error (%s): %s\n", p.Error.Code, p.Error.Message)
	}
}

var cmdCollectionPut = &command{
	name:    "put",
	args:    "<collection> <path> --meta meta.json [[name=]file|dir]... [--url name=url]... [--checksum name=multihash]... [--nocopy] [--upload] [--replace] [--wait] | <collection> --batch items.ndjson",
	summary: "push an item (files, local paths or URLs) into this node's writer head",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		metaFile := fs.String("meta", "", "metadata record: a JSON object file")
		urls := map[string]string{}
		fs.Func("url", "name=url: a file fetched by URL import (repeatable)", func(s string) error {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				return fmt.Errorf("want name=url")
			}
			urls[k] = v
			return nil
		})
		sums := map[string]string{}
		fs.Func("checksum", "name=multihash: verify a URL file (hex multihash, e.g. STAC file:checksum)", func(s string) error {
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				return fmt.Errorf("want name=multihash")
			}
			sums[k] = v
			return nil
		})
		nocopy := fs.Bool("nocopy", false, "reference local files in place on the receiving node")
		upload := fs.Bool("upload", false, "upload local files instead of passing their paths (automatic with a remote --api)")
		replace := fs.Bool("replace", false, "supersede a different entry at the path in this node's writer head")
		wait := fs.Bool("wait", false, "wait until the item is written (or failed), up to 5 minutes")
		batch := fs.String("batch", "", "NDJSON file of push requests (one {path, meta, files} per line)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		p, err := e.pushes(ctx)
		if err != nil {
			return err
		}
		if *batch != "" {
			if fs.NArg() != 1 {
				return usagef("collection put --batch takes only the collection name")
			}
			return putBatch(ctx, e, p, fs.Arg(0), *batch)
		}
		if fs.NArg() < 2 || *metaFile == "" {
			return usagef("collection put needs <collection> <path> --meta meta.json and files")
		}
		name, path := fs.Arg(0), fs.Arg(1)
		meta, err := os.ReadFile(*metaFile)
		if err != nil {
			return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", *metaFile, err)
		}
		local, err := pushFiles(fs.Args()[2:])
		if err != nil {
			return err
		}
		req := apiv0.CollectionPushRequest{Path: path, Meta: meta, Replace: *replace, Files: map[string]apiv0.CollectionPushFile{}}
		for k, u := range urls {
			req.Files[k] = apiv0.CollectionPushFile{URL: u, Checksum: sums[k]}
		}
		for k := range sums {
			if _, ok := urls[k]; !ok {
				return usagef("--checksum %s names no --url file", k)
			}
		}
		send := *upload || e.remoteAPI()
		if send && *nocopy {
			return usagef("--nocopy references files on the receiving node; it cannot be combined with uploads")
		}
		for k, lp := range local {
			if _, dup := req.Files[k]; dup {
				return usagef("file %q given twice", k)
			}
			if !send {
				req.Files[k] = apiv0.CollectionPushFile{Path: lp, NoCopy: *nocopy}
				continue
			}
			f, err := os.Open(lp)
			if err != nil {
				return err
			}
			fi, _ := f.Stat()
			u, err := p.CollectionUpload(ctx, name, f, fi.Size())
			f.Close()
			if err != nil {
				return err
			}
			req.Files[k] = apiv0.CollectionPushFile{Upload: u.CID}
		}
		var w = apiv0.MaxPushWait
		if !*wait {
			w = 0
		}
		res, err := p.CollectionPush(ctx, name, req, w)
		if err != nil {
			return err
		}
		if err := e.emit(res, func(w io.Writer) { printPush(w, res) }); err != nil {
			return err
		}
		if res.State == apiv0.PushFailed && res.Error != nil {
			return res.Error
		}
		return nil
	},
}

func putBatch(ctx context.Context, e *env, p node.PushNode, name, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", file, err)
	}
	defer f.Close()
	var reqs []apiv0.CollectionPushRequest
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r apiv0.CollectionPushRequest
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return usagef("%s line %d: %v", file, len(reqs)+1, err)
		}
		reqs = append(reqs, r)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	lines, err := p.CollectionPushBatch(ctx, name, reqs)
	if err != nil {
		return err
	}
	failed := 0
	for _, l := range lines {
		if l.Error != nil {
			failed++
		}
	}
	if err := e.emit(lines, func(w io.Writer) {
		for _, l := range lines {
			if l.Error != nil {
				fmt.Fprintf(w, "line %d %s: rejected (%s): %s\n", l.Line, l.Path, l.Error.Code, l.Error.Message)
			} else {
				printPush(w, l.Push)
			}
		}
	}); err != nil {
		return err
	}
	if failed > 0 {
		return apiv0.Errorf(apiv0.CodeFailed, "%d of %d pushes were rejected", failed, len(lines))
	}
	return nil
}

var cmdCollectionRm = &command{
	name:    "rm",
	args:    "<collection> <path> [--wait]",
	summary: "remove an item from this node's writer head",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		wait := fs.Bool("wait", false, "wait until the removal is in the writer head")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return usagef("collection rm needs <collection> <path>")
		}
		p, err := e.pushes(ctx)
		if err != nil {
			return err
		}
		var w = apiv0.MaxPushWait
		if !*wait {
			w = 0
		}
		res, err := p.CollectionRemove(ctx, fs.Arg(0), fs.Arg(1), w)
		if err != nil {
			return err
		}
		return e.emit(res, func(w io.Writer) { printPush(w, res) })
	},
}

var cmdCollectionPushes = &command{
	name:    "pushes",
	args:    "<collection> [--since <cursor>|0] [--limit n]",
	summary: "follow this node's push log: each pushed path with its latest state",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		since := fs.String("since", "0", "push log cursor (0: from the start)")
		limit := fs.Int("limit", 0, "page size (default 100, max 1000)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if err := oneName(fs, "collection pushes"); err != nil {
			return err
		}
		p, err := e.pushes(ctx)
		if err != nil {
			return err
		}
		page, err := p.CollectionPushes(ctx, fs.Arg(0), *since, *limit)
		if err != nil {
			return err
		}
		return e.emit(page, func(w io.Writer) {
			if page.Resync {
				fmt.Fprintln(w, "the cursor is from another collection database; list again with --since 0")
				return
			}
			for i := range page.Items {
				fmt.Fprintf(w, "%d ", page.Items[i].Seq)
				printPush(w, &page.Items[i])
			}
			fmt.Fprintf(w, "next: --since %s\n", page.NextCursor)
		})
	},
}

func tokenErr(err error, name string) error {
	switch {
	case errors.Is(err, token.ErrNotFound):
		return apiv0.Errorf(apiv0.CodeNotFound, "no token %q", name).WithAction("list tokens with `" + nu.Name + " api token ls`")
	case errors.Is(err, token.ErrExists):
		return apiv0.Errorf(apiv0.CodeConflict, "token %q exists", name).WithAction("rotate it, or pick another name")
	case err != nil:
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	return nil
}

func (e *env) tokenDir() (string, error) {
	r, err := e.openRepo()
	if err != nil {
		return "", err
	}
	return r.Path(token.Dir), nil
}

func printToken(w io.Writer, t *token.Token, secret bool) {
	fmt.Fprintf(w, "%s: %s into %s (created %s)\n", t.Name, t.Scope, strings.Join(t.Collections, ", "), t.Created.Format("2006-01-02 15:04"))
	if secret {
		fmt.Fprintf(w, "token: %s\n", t.Secret)
		fmt.Fprintf(w, "producers use it with `%s --api <host>:<port> --token <token> cluster collection put …` against api.producer.listen\n", nu.Name)
	}
}

var cmdAPI = &command{
	name:    "api",
	summary: "API credentials for producers on other machines",
	sub: []*command{{
		name:    "token",
		summary: "push tokens scoped to named collections (served on api.producer.listen)",
		sub: []*command{
			{
				name:    "create",
				args:    "<name> --collection <collection>...",
				summary: "create a push token and print it",
				run: func(ctx context.Context, e *env, args []string) error {
					fs := e.flags()
					var cols []string
					fs.Func("collection", "collection the token may push into (repeatable)", func(s string) error { cols = append(cols, s); return nil })
					if err := parseFlags(fs, args); err != nil {
						return err
					}
					if fs.NArg() != 1 || len(cols) == 0 {
						return usagef("api token create needs <name> and at least one --collection")
					}
					dir, err := e.tokenDir()
					if err != nil {
						return err
					}
					t, err := token.Create(dir, fs.Arg(0), cols)
					if err != nil {
						return tokenErr(err, fs.Arg(0))
					}
					return e.emit(t, func(w io.Writer) { printToken(w, t, true) })
				},
			},
			{
				name:    "ls",
				summary: "list push tokens (without their secrets)",
				run: func(ctx context.Context, e *env, args []string) error {
					fs := e.flags()
					if err := parseFlags(fs, args); err != nil {
						return err
					}
					dir, err := e.tokenDir()
					if err != nil {
						return err
					}
					ts, err := token.List(dir)
					if err != nil {
						return err
					}
					for i := range ts {
						ts[i].Secret = ""
					}
					if ts == nil {
						ts = []token.Token{}
					}
					return e.emit(ts, func(w io.Writer) {
						if len(ts) == 0 {
							fmt.Fprintln(w, "no push tokens")
						}
						for i := range ts {
							printToken(w, &ts[i], false)
						}
					})
				},
			},
			{
				name:    "rotate",
				args:    "<name>",
				summary: "replace a token's secret (the old one stops working at once)",
				run: func(ctx context.Context, e *env, args []string) error {
					fs := e.flags()
					if err := parseFlags(fs, args); err != nil {
						return err
					}
					if fs.NArg() != 1 {
						return usagef("api token rotate needs <name>")
					}
					dir, err := e.tokenDir()
					if err != nil {
						return err
					}
					t, err := token.Rotate(dir, fs.Arg(0))
					if err != nil {
						return tokenErr(err, fs.Arg(0))
					}
					return e.emit(t, func(w io.Writer) { printToken(w, t, true) })
				},
			},
			{
				name:    "rm",
				args:    "<name>",
				summary: "revoke a token",
				run: func(ctx context.Context, e *env, args []string) error {
					fs := e.flags()
					if err := parseFlags(fs, args); err != nil {
						return err
					}
					if fs.NArg() != 1 {
						return usagef("api token rm needs <name>")
					}
					dir, err := e.tokenDir()
					if err != nil {
						return err
					}
					if err := token.Remove(dir, fs.Arg(0)); err != nil {
						return tokenErr(err, fs.Arg(0))
					}
					return e.emit(map[string]string{"removed": fs.Arg(0)}, func(w io.Writer) { fmt.Fprintf(w, "revoked %s\n", fs.Arg(0)) })
				},
			},
		},
	}},
}
