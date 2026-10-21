package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/target"
)

// Content commands. They parse their input here so users get the same errors with and
// without a daemon, then call the node.

var cmdAdd = &command{
	name:    "add",
	args:    "[-r] [--raw] [--hash sha256] [--nocopy] [--no-announce] [--no-wait] <path>",
	summary: "add a file or directory to the local store and print its CID",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		req := apiv0.AddRequest{Pin: true}
		fs.BoolVar(&req.Recursive, "r", false, "add a directory recursively")
		fs.BoolVar(&req.Raw, "raw", false, "whole-file blob CID instead of a file node with metadata")
		fs.StringVar(&req.Hash, "hash", "", "blob hash: blake3 (default) or sha256 (core DASL, ≤ 8 MiB)")
		fs.BoolVar(&req.NoCopy, "nocopy", false, "reference the local file instead of copying it")
		fs.BoolVar(&req.NoAnnounce, "no-announce", false, "do not announce the content to the network")
		noWait := fs.Bool("no-wait", false, "print the operation and return instead of following it")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("add needs exactly one path")
		}
		if req.Hash != "" && req.Hash != "sha256" && req.Hash != "blake3" {
			return usagef("--hash must be blake3 or sha256")
		}
		if req.Hash == "blake3" {
			req.Hash = ""
		}
		p, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		fi, err := os.Stat(p)
		if err != nil {
			return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", fs.Arg(0), err).WithAction("check the path")
		}
		if fi.IsDir() && !req.Recursive {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a directory", fs.Arg(0)).WithAction("add it with -r")
		}
		req.Path = p
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		op, err := n.Add(ctx, req)
		if err != nil {
			return err
		}
		return finishOperation(e, ctx, n, op, *noWait, func(w io.Writer, res apiv0.AddResult) {
			fmt.Fprintf(w, "added %s %s\n", res.CID, res.Name)
			for _, s := range res.Skipped {
				fmt.Fprintf(w, "skipped %s\n", s)
			}
			if res.Link != "" {
				fmt.Fprintf(w, "share: %s\n", res.Link)
			}
		})
	},
}

// finishOperation either prints the started operation (noWait) or follows it and prints
// its result with print (text mode) or as JSON.
func finishOperation[T any](e *env, ctx context.Context, n node.Node, op *apiv0.Operation, noWait bool, print func(io.Writer, T)) error {
	if noWait {
		return e.emit(op, func(w io.Writer) {
			fmt.Fprintf(w, "operation %s started; follow it with `%s operations wait %s`\n", op.ID, nu.Name, op.ID)
		})
	}
	final, err := e.follow(ctx, n, op.ID)
	if err != nil {
		return err
	}
	if e.json {
		return nil // the done event already carried the result
	}
	var res T
	if err := json.Unmarshal(final.Result, &res); err != nil {
		return apiv0.Errorf(apiv0.CodeFailed, "malformed result: %v", err)
	}
	print(e.stdout, res)
	return nil
}

var cmdCat = &command{
	name:    "cat",
	args:    "<cid[/path]>",
	summary: "write a file's bytes to stdout",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		t, err := oneTarget(fs.Args())
		if err != nil {
			return err
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		rc, err := n.Cat(ctx, t.Raw)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(e.stdout, rc)
		return err
	},
}

var cmdLs = &command{
	name:    "ls",
	args:    "<cid[/path]>",
	summary: "list a directory (or describe a file)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		t, err := oneTarget(fs.Args())
		if err != nil {
			return err
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		l, err := n.Ls(ctx, t.Raw)
		if err != nil {
			return err
		}
		return e.emit(l, func(w io.Writer) {
			for _, en := range l.Entries {
				fmt.Fprintf(w, "%s\t%d\t%s\n", en.CID, en.Size, en.Name)
			}
		})
	},
}

var cmdGet = &command{
	name:    "get",
	args:    "<target> [-o path]",
	summary: "fetch a CID or link to disk (not available in this build)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		fs.String("o", "", "output path")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if _, err := oneTarget(fs.Args()); err != nil {
			return err
		}
		if _, err := e.nodeOrLocal(ctx); err != nil {
			return err
		}
		return apiv0.NotImplemented("get")
	},
}

// oneTarget parses the single target argument and requires a supported form.
func oneTarget(args []string) (target.Target, error) {
	if len(args) != 1 {
		return target.Target{}, usagef("expected one target: %s", target.Forms)
	}
	t, err := target.Parse(args[0])
	if err != nil {
		return t, err
	}
	return t, t.Supported()
}

var cmdPin = &command{
	name:    "pin",
	summary: "keep content in the local store",
	sub: []*command{
		{
			name:    "add",
			args:    "[--no-wait] <cid>",
			summary: "pin held content so GC keeps it",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				noWait := fs.Bool("no-wait", false, "print the operation and return instead of following it")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || !target.IsCID(fs.Arg(0)) {
					return usagef("pin add needs one CID")
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				op, err := n.PinAdd(ctx, apiv0.PinRequest{CID: fs.Arg(0)})
				if err != nil {
					return err
				}
				return finishOperation(e, ctx, n, op, *noWait, func(w io.Writer, p apiv0.Pin) {
					fmt.Fprintf(w, "pinned %s\n", p.CID)
				})
			},
		},
		{
			name:    "rm",
			args:    "<cid>",
			summary: "remove an explicit pin",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || !target.IsCID(fs.Arg(0)) {
					return usagef("pin rm needs one CID")
				}
				n, err := e.nodeOrLocal(ctx)
				if err != nil {
					return err
				}
				if err := n.PinRm(ctx, fs.Arg(0)); err != nil {
					return err
				}
				return e.emit(apiv0.Pin{CID: fs.Arg(0)}, func(w io.Writer) { fmt.Fprintf(w, "unpinned %s\n", fs.Arg(0)) })
			},
		},
		{
			name:    "ls",
			summary: "list pins",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				n, err := e.nodeOrLocal(ctx)
				if err != nil {
					return err
				}
				return listAll(e, func(cursor string) (*apiv0.Page[apiv0.Pin], error) {
					return n.Pins(ctx, cursor, 0)
				}, func(w io.Writer, p apiv0.Pin) { fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.CID, p.Mode, p.Source, p.Name) })
			},
		},
	},
}

var cmdDag = &command{
	name:    "dag",
	summary: "read and write DRISL objects as JSON",
	sub: []*command{
		{
			name:    "get",
			args:    "<cid[/path]>",
			summary: "print a DRISL object (or a path into it) as atproto-style JSON",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				t, err := oneTarget(fs.Args())
				if err != nil {
					return err
				}
				n, err := e.nodeOrLocal(ctx)
				if err != nil {
					return err
				}
				doc, err := n.DagGet(ctx, t.Raw)
				if err != nil {
					return err
				}
				_, err = e.stdout.Write(append(doc, '\n'))
				return err
			},
		},
		{
			name:    "put",
			args:    "[file]",
			summary: "store atproto-style JSON (stdin by default) as one SHA-256 DRISL object",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				var in io.Reader = os.Stdin
				if fs.NArg() == 1 {
					f, err := os.Open(fs.Arg(0))
					if err != nil {
						return apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", fs.Arg(0), err)
					}
					defer f.Close()
					in = f
				} else if fs.NArg() > 1 {
					return usagef("dag put takes at most one file")
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				res, err := n.DagPut(ctx, in)
				if err != nil {
					return err
				}
				return e.emit(res, func(w io.Writer) { fmt.Fprintln(w, res.CID) })
			},
		},
	},
}

var cmdOperations = &command{
	name:    "operations",
	summary: "list and follow long-running operations",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		n, err := e.node(ctx)
		if err != nil {
			return err
		}
		return listAll(e, func(cursor string) (*apiv0.Page[apiv0.Operation], error) {
			return n.Operations(ctx, cursor, 0)
		}, func(w io.Writer, o apiv0.Operation) {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", o.ID, o.Kind, o.State, o.Stage, o.Target)
		})
	},
	sub: []*command{
		{
			name:    "get",
			args:    "<id>",
			summary: "show one operation",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("operations get needs one operation ID")
				}
				n, err := e.node(ctx)
				if err != nil {
					return err
				}
				o, err := n.Operation(ctx, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(o, func(w io.Writer) { printOperation(w, o) })
			},
		},
		{
			name:    "wait",
			args:    "[--timeout d] <id>",
			summary: "follow an operation's progress until it finishes",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				timeout := fs.Duration("timeout", 0, "give up after this long (0: never)")
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("operations wait needs one operation ID")
				}
				n, err := e.node(ctx)
				if err != nil {
					return err
				}
				if *timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, *timeout)
					defer cancel()
				}
				final, err := e.follow(ctx, n, fs.Arg(0))
				if err != nil {
					if ctx.Err() != nil && *timeout > 0 {
						return apiv0.Errorf(apiv0.CodeTimeout, "operation %s did not finish within %s", fs.Arg(0), *timeout).
							WithAction("wait again, or cancel it with `" + nu.Name + " operations cancel " + fs.Arg(0) + "`")
					}
					return err
				}
				if e.json {
					return nil
				}
				printOperation(e.stdout, final)
				return nil
			},
		},
		{
			name:    "cancel",
			args:    "<id>",
			summary: "cancel a running operation",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("operations cancel needs one operation ID")
				}
				n, err := e.node(ctx)
				if err != nil {
					return err
				}
				if err := n.CancelOperation(ctx, fs.Arg(0)); err != nil {
					return err
				}
				o, err := n.Operation(ctx, fs.Arg(0))
				if err != nil {
					return err
				}
				return e.emit(o, func(w io.Writer) { printOperation(w, o) })
			},
		},
	},
}

func printOperation(w io.Writer, o *apiv0.Operation) {
	fmt.Fprintf(w, "%s %s %s\n  state: %s", o.ID, o.Kind, o.Target, o.State)
	if o.Stage != "" {
		fmt.Fprintf(w, " (%s)", o.Stage)
	}
	fmt.Fprintln(w)
	if o.Progress.Done > 0 || o.Progress.Total > 0 {
		fmt.Fprintf(w, "  progress: %d/%d %s\n", o.Progress.Done, o.Progress.Total, o.Progress.Unit)
	}
	fmt.Fprintf(w, "  updated: %s\n", o.Updated.Local().Format(time.RFC3339))
	if len(o.Result) > 0 {
		fmt.Fprintf(w, "  result: %s\n", o.Result)
	}
	if o.Error != nil {
		fmt.Fprintf(w, "  error: %s\n", o.Error.Message)
	}
}

// listAll walks every page of a list, honouring resync by starting over, and prints rows
// (text) or the collected items (JSON).
func listAll[T any](e *env, next func(cursor string) (*apiv0.Page[T], error), row func(io.Writer, T)) error {
	var items []T
	cursor := ""
	for attempts := 0; ; attempts++ {
		page, err := next(cursor)
		if err != nil {
			return err
		}
		if page.Resync {
			if attempts > 3 {
				return apiv0.Errorf(apiv0.CodeFailed, "the list keeps changing").WithAction("try again")
			}
			items, cursor = nil, ""
			continue
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if items == nil {
		items = []T{}
	}
	return e.emit(items, func(w io.Writer) {
		for _, it := range items {
			row(w, it)
		}
	})
}
