package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// Login, public names, node registration and follows (0045; docs/names.md, docs/follow.md).

func namesNode(n node.Node) (node.NamesNode, error) {
	nn, ok := n.(node.NamesNode)
	if !ok {
		return nil, apiv0.NotImplemented("names, login and follows")
	}
	return nn, nil
}

func jsonLine(v any) ([]byte, error) { return json.Marshal(v) }

// decodeResult decodes a finished operation's result.
func decodeResult(o *apiv0.Operation, out any) error {
	if err := json.Unmarshal(o.Result, out); err != nil {
		return apiv0.Errorf(apiv0.CodeFailed, "bad operation result: %v", err)
	}
	return nil
}

// openBrowser tries the platform opener; failure is fine, the URL is printed anyway.
func openBrowser(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() == nil
}

var cmdLogin = &command{
	name:    "login",
	args:    "<handle-or-did> [--no-browser]",
	summary: "log in to an atproto account (OAuth; Nu record permissions only) so you can publish names",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		noBrowser := fs.Bool("no-browser", false, "print the authorization URL instead of opening a browser")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("login needs a handle or DID")
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		start, err := nn.Login(ctx, apiv0.LoginRequest{Identifier: fs.Arg(0)})
		if err != nil {
			return err
		}
		opened := !*noBrowser && openBrowser(start.URL)
		if !e.json {
			if opened {
				fmt.Fprintf(e.stderr, "%s: opened a browser to authorize %s; if it did not open, visit:\n%s\n", nu.Name, start.DID, start.URL)
			} else {
				fmt.Fprintf(e.stderr, "%s: open this URL in a browser to authorize %s:\n%s\n", nu.Name, start.DID, start.URL)
			}
		} else {
			b, _ := jsonLine(start)
			fmt.Fprintln(e.stdout, string(b))
		}
		final, err := n.WaitOperation(ctx, start.Operation.ID, 0)
		if err != nil {
			return err
		}
		if final.State != apiv0.StateSucceeded {
			if final.Error != nil {
				return final.Error
			}
			return apiv0.Errorf(apiv0.CodeFailed, "login %s", final.State)
		}
		var acct apiv0.Account
		if err := decodeResult(final, &acct); err != nil {
			return err
		}
		return e.emit(acct, func(w io.Writer) {
			fmt.Fprintf(w, "logged in as %s (%s) on %s\n  scopes: %s\n", acct.Handle, acct.DID, acct.PDS, strings.Join(acct.Scopes, " "))
		})
	},
}

var cmdLogout = &command{
	name:    "logout",
	args:    "[<handle-or-did>]",
	summary: "revoke and forget a login (the only one when unnamed)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() > 1 {
			return usagef("logout takes at most one account")
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		if err := nn.Logout(ctx, fs.Arg(0)); err != nil {
			return err
		}
		if !e.json {
			fmt.Fprintln(e.stdout, "logged out")
		}
		return nil
	},
}

func printResolution(w io.Writer, r *apiv0.NameResolution) {
	held := ""
	if r.Held {
		held = " (held here)"
	}
	fmt.Fprintf(w, "%s\t%s\t%s%s\n", r.Name, r.Target, r.Kind, held)
	fmt.Fprintf(w, "  %s rev %s, %s answer %s old (identity %s old)\n", r.URI, r.Rev, r.Source, r.Age, r.IdentityAge)
	if r.Source == "cache" {
		fmt.Fprintf(w, "  cached: no name-plane request was made; pass --fresh to ask the PDS\n")
	}
}

var cmdPublish = &command{
	name:    "publish",
	args:    "<name> <cid> [--kind tree|file|blob] [--note text] [--as account] [--wait-seed peer]... [--timeout 10m]",
	summary: "point a public name at a CID (a name write; NOT replication readiness unless --wait-seed is satisfied)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		kind := fs.String("kind", "", "tree, file or blob (detected from held content)")
		note := fs.String("note", "", "note stored in the record")
		as := fs.String("as", "", "account (handle or DID) when several are logged in")
		var seeds []string
		fs.Func("wait-seed", "seed PeerId (a storage member of your cluster) whose fresh complete report for exactly this CID to wait for; repeatable", func(s string) error { seeds = append(seeds, s); return nil })
		timeout := fs.String("timeout", "", "seed wait timeout (default 10m; 0 waits indefinitely)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return usagef("publish needs a name and a CID")
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		res, err := nn.Publish(ctx, apiv0.PublishRequest{Name: fs.Arg(0), Target: fs.Arg(1), Kind: *kind, Note: *note, Account: *as, WaitSeed: seeds, Timeout: *timeout})
		if res != nil {
			if eerr := e.emit(res, func(w io.Writer) { printPublish(w, res) }); eerr != nil {
				return eerr
			}
		}
		return err
	},
}

func printPublish(w io.Writer, res *apiv0.PublishResult) {
	p := res.Publication
	fmt.Fprintf(w, "published %s → %s (%s)\n  %s rev %s\n", p.Name, p.Target, p.Kind, p.URI, p.Rev)
	if p.Dataset != "" {
		fmt.Fprintf(w, "  dataset %s follows it here\n", p.Dataset)
	}
	if res.Wait == nil {
		fmt.Fprintf(w, "  %s\n", res.Note)
		return
	}
	fmt.Fprintf(w, "seed wait for exactly %s:\n", res.Wait.Target)
	printWait(w, res.Wait)
	if res.Wait.Satisfied {
		fmt.Fprintf(w, "  the seed holds it; this machine may go offline (W5)\n")
	}
}

var cmdResolve = &command{
	name:    "resolve",
	args:    "<handle-or-did>/<name> [--fresh] [--offline] [--max-age 1h]",
	summary: "resolve a public name to its verified target and show how fresh the answer is",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		fresh := fs.Bool("fresh", false, "ask the PDS even when a cached answer exists")
		offline := fs.Bool("offline", false, "answer only from the cache (any age)")
		maxAge := fs.String("max-age", "", "accept a cached answer at most this old")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("resolve needs one name")
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		r, err := nn.ResolveName(ctx, fs.Arg(0), apiv0.ResolveRequest{Fresh: *fresh, Offline: *offline, MaxAge: *maxAge})
		if err != nil {
			return err
		}
		return e.emit(r, func(w io.Writer) { printResolution(w, r) })
	},
}

var cmdNames = &command{
	name:    "names",
	args:    "[--as account]",
	summary: "list your published names (each verified)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		as := fs.String("as", "", "account (handle or DID); the logged-in one by default")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		l, err := nn.Names(ctx, *as)
		if err != nil {
			return err
		}
		return e.emit(l, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s): %d name(s)\n", l.Handle, l.DID, len(l.Names))
			for i := range l.Names {
				printResolution(w, &l.Names[i])
			}
		})
	},
}

var cmdUnpublish = &command{
	name:    "unpublish",
	args:    "<name> [--as account]",
	summary: "delete a public name record (followers keep their last target as a visible orphan)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		as := fs.String("as", "", "account (handle or DID)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("unpublish needs one name")
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		p, err := nn.Unpublish(ctx, fs.Arg(0), *as)
		if err != nil {
			return err
		}
		return e.emit(p, func(w io.Writer) { fmt.Fprintf(w, "unpublished %s (rev %s)\n", p.URI, p.Rev) })
	},
}

var cmdNode = &command{
	name:    "node",
	summary: "this node's record in your account",
	sub: []*command{{
		name:    "register",
		args:    "[--label name] [--seed] [--as account]",
		summary: "write this node's record (role device, plus seed with --seed) into your account",
		run: func(ctx context.Context, e *env, args []string) error {
			fs := e.flags()
			label := fs.String("label", "", "label (default: the host name)")
			seed := fs.Bool("seed", false, "also claim the seed role")
			as := fs.String("as", "", "account (handle or DID)")
			if err := parseFlags(fs, args); err != nil {
				return err
			}
			n, err := e.nodeOrLocalWrite(ctx)
			if err != nil {
				return err
			}
			nn, err := namesNode(n)
			if err != nil {
				return err
			}
			r, err := nn.NodeRegister(ctx, apiv0.NodeRegisterRequest{Label: *label, Seed: *seed, Account: *as})
			if err != nil {
				return err
			}
			return e.emit(r, func(w io.Writer) {
				fmt.Fprintf(w, "registered %s as %q (%s)\n  %s\n", r.Peer, r.Label, strings.Join(r.Roles, ","), r.URI)
			})
		},
	}},
}

var cmdDevices = &command{
	name:    "devices",
	args:    "[--as account]",
	summary: "list the nodes registered in an account",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		as := fs.String("as", "", "account (handle or DID); the logged-in one by default")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		l, err := nn.Devices(ctx, *as)
		if err != nil {
			return err
		}
		return e.emit(l, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s): %d device(s)\n", l.Handle, l.DID, len(l.Devices))
			for _, d := range l.Devices {
				self := ""
				if d.Self {
					self = " (this node)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s%s\n", d.Peer, d.Label, strings.Join(d.Roles, ","), self)
			}
		})
	},
}

func printFollow(w io.Writer, f *apiv0.Follow) {
	fmt.Fprintf(w, "%s\t%s\t%s\t%s", f.Dataset, f.URI, f.Mode, f.State)
	if f.Detail != "" {
		fmt.Fprintf(w, " (%s)", f.Detail)
	}
	fmt.Fprintln(w)
	root := func(label string, r *apiv0.FollowRoot) {
		if r == nil {
			return
		}
		fmt.Fprintf(w, "  %-9s %s", label, r.Target)
		if r.Rev != "" {
			fmt.Fprintf(w, " rev %s", r.Rev)
		}
		if r.Age != "" {
			fmt.Fprintf(w, " (%s old)", r.Age)
		}
		fmt.Fprintln(w)
	}
	root("observed:", f.Observed)
	root("syncing:", f.Syncing)
	root("held:", f.Held)
	for _, b := range f.Blockers {
		fmt.Fprintf(w, "  blocker: %s\n", b)
	}
	for _, o := range f.Orphans {
		fmt.Fprintf(w, "  orphan pin: %s\n", o)
	}
	if f.Error != "" {
		fmt.Fprintf(w, "  last refresh error: %s\n", f.Error)
	}
}

var cmdFollow = &command{
	name:    "follow",
	args:    "<handle-or-did>/<name> [--dataset ds] [--cluster] [--all] [--replicas k] [--max-size n] [--no-announce]",
	summary: "mirror a public name as a local dataset that tracks its current target (`follow ls`, `follow refresh`)",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		ds := fs.String("dataset", "", "dataset name (derived from the handle and name by default)")
		inCluster := fs.Bool("cluster", false, "follow in the networked cluster (storage members hold it) instead of this node alone")
		all := fs.Bool("all", false, "cluster: every storage member holds every blob (the seed layout)")
		replicas := fs.Int64("replicas", 0, "cluster: copies per blob")
		maxSize := fs.String("max-size", "", "logical byte quota; larger publications pause the follow")
		noAnnounce := fs.Bool("no-announce", false, "follow without announcing (not privacy)")
		path := fs.String("path", "", "scoped follow (not supported yet)")
		lazy := fs.Bool("lazy", false, "lazy follow (not supported yet)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("follow needs one name (or `follow ls`)")
		}
		var size int64
		if *maxSize != "" {
			var err error
			if size, err = parseSize(*maxSize); err != nil {
				return usagef("--max-size: %v", err)
			}
		}
		scope := node.ScopeLocal
		if *inCluster {
			scope = node.ScopeCluster
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		f, err := nn.Follow(ctx, apiv0.FollowRequest{Target: fs.Arg(0), Dataset: *ds, Scope: scope, All: *all, Replicas: *replicas, MaxSize: size, NoAnnounce: *noAnnounce, Path: *path, Lazy: *lazy})
		if err != nil {
			return err
		}
		return e.emit(f, func(w io.Writer) { printFollow(w, f) })
	},
	sub: []*command{
		{
			name:    "ls",
			summary: "list follows: observed/syncing/held roots, state, lag and blockers",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				n, err := e.nodeOrLocal(ctx)
				if err != nil {
					return err
				}
				nn, err := namesNode(n)
				if err != nil {
					return err
				}
				fl, err := nn.Follows(ctx)
				if err != nil {
					return err
				}
				return e.emit(fl, func(w io.Writer) {
					for i := range fl {
						printFollow(w, &fl[i])
					}
				})
			},
		},
		{
			name:    "refresh",
			args:    "[<dataset-or-name>]",
			summary: "re-resolve follows from their PDSes now",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				nn, err := namesNode(n)
				if err != nil {
					return err
				}
				fl, err := nn.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: fs.Arg(0)})
				if err != nil {
					return err
				}
				return e.emit(fl, func(w io.Writer) {
					for i := range fl {
						printFollow(w, &fl[i])
					}
				})
			},
		},
	},
}

var cmdUnfollow = &command{
	name:    "unfollow",
	args:    "<dataset-or-name>",
	summary: "stop following: the follow's dataset and its pins go; other pins of the content stay",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("unfollow needs one dataset or name")
		}
		n, err := e.nodeOrLocalWrite(ctx)
		if err != nil {
			return err
		}
		nn, err := namesNode(n)
		if err != nil {
			return err
		}
		f, err := nn.Unfollow(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return e.emit(f, func(w io.Writer) { printFollow(w, f) })
	},
}
