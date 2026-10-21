package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/node/repo"
)

var cmdVersion = &command{
	name:    "version",
	summary: "print the version",
	run: func(ctx context.Context, e *env, args []string) error {
		if err := parseFlags(e.flags(), args); err != nil {
			return err
		}
		return e.emit(map[string]string{"name": nu.Name, "version": nu.Version}, func(w io.Writer) {
			fmt.Fprintf(w, "%s %s\n", nu.Name, nu.Version)
		})
	},
}

var cmdInit = &command{
	name:    "init",
	summary: "initialize $" + repo.EnvPath + " (any command does this on first use)",
	run: func(ctx context.Context, e *env, args []string) error {
		if err := parseFlags(e.flags(), args); err != nil {
			return err
		}
		path := e.nuPath
		if path == "" {
			var err error
			if path, err = repo.DefaultPath(); err != nil {
				return err
			}
		}
		existed := repo.Exists(path)
		r, err := e.openRepo()
		if err != nil {
			return err
		}
		// A complete repo has an identity: create node.key now so the first share link
		// and the first daemon agree on the PeerId.
		key, err := node.LoadKey(r)
		if err != nil {
			return err
		}
		peer := key.Public().PeerID().Base36()
		return e.emit(map[string]any{"root": r.Root(), "created": !existed, "peer_id": peer}, func(w io.Writer) {
			if existed {
				fmt.Fprintf(w, "already initialized at %s\n", r.Root())
			} else {
				fmt.Fprintf(w, "initialized %s\n", r.Root())
			}
			fmt.Fprintf(w, "peer id: %s\n", peer)
		})
	},
}

var cmdConfig = &command{
	name:    "config",
	summary: "show or edit config.toml (dotted keys such as api.listen)",
	sub: []*command{
		{
			name:    "get",
			args:    "<key>",
			summary: "print one value",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 {
					return usagef("config get needs exactly one key")
				}
				r, err := e.openRepo()
				if err != nil {
					return err
				}
				v, err := r.ConfigGet(fs.Arg(0))
				if err != nil {
					return apiv0.Errorf(apiv0.CodeNotFound, "%v", err).WithAction("list keys with `" + nu.Name + " config`")
				}
				return e.emit(repo.KeyValue{Key: fs.Arg(0), Value: v}, func(w io.Writer) {
					printConfigValue(w, "", v)
				})
			},
		},
		{
			name:    "set",
			args:    "<key> <value>",
			summary: "set one value (parsed as TOML when possible, else a string)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 2 {
					return usagef("config set needs a key and a value")
				}
				r, err := e.openRepo()
				if err != nil {
					return err
				}
				if err := r.ConfigSet(fs.Arg(0), fs.Arg(1)); err != nil {
					return apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
				}
				v, _ := r.ConfigGet(fs.Arg(0))
				return e.emit(repo.KeyValue{Key: fs.Arg(0), Value: v}, func(w io.Writer) {
					fmt.Fprintf(w, "%s = %s\n", fs.Arg(0), formatValue(v))
					fmt.Fprintf(w, "(a running daemon applies it after restart)\n")
				})
			},
		},
	},
	run: runConfigShow,
}

func runConfigShow(ctx context.Context, e *env, args []string) error {
	if err := parseFlags(e.flags(), args); err != nil {
		return err
	}
	r, err := e.openRepo()
	if err != nil {
		return err
	}
	m, err := r.ConfigAll()
	if err != nil {
		return err
	}
	return e.emit(m, func(w io.Writer) {
		for _, kv := range repo.FlattenConfig(m) {
			fmt.Fprintf(w, "%s = %s\n", kv.Key, formatValue(kv.Value))
		}
	})
}

func printConfigValue(w io.Writer, prefix string, v any) {
	if m, ok := v.(map[string]any); ok {
		for _, kv := range repo.FlattenConfig(m) {
			fmt.Fprintf(w, "%s = %s\n", kv.Key, formatValue(kv.Value))
		}
		return
	}
	fmt.Fprintln(w, formatValue(v))
}

func formatValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = formatValue(p)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

var cmdDaemon = &command{
	name:    "daemon",
	summary: "run the node: libp2p swarm, local API and loopback gateway",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		apiListen := fs.String("api-listen", "", "override api.listen (host:port)")
		gwListen := fs.String("gateway-listen", "", "override gateway.listen (host:port)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		r, err := e.openRepo()
		if err != nil {
			return err
		}
		log := slog.New(slog.NewTextHandler(e.stderr, nil))
		d, err := daemon.Start(ctx, daemon.Options{Repo: r, APIListen: *apiListen, GatewayListen: *gwListen, Logger: log})
		if err != nil {
			return err
		}
		st, _ := d.Node.Status(ctx)
		if err := e.emit(st, func(w io.Writer) {
			fmt.Fprintf(w, "%s %s serving %s\n  peer id  %s\n  swarm    %s\n  API      http://%s%s\n  gateway  http://%s/\npress Ctrl-C to stop\n",
				nu.Name, nu.Version, r.Root(), d.Node.PeerID().Base36(), strings.Join(st.Network.Addrs, ", "), d.APIAddr(), apiv0.Prefix, d.GatewayAddr())
		}); err != nil {
			d.Close()
			return err
		}
		return d.Wait(ctx)
	},
}

// Shared by status and doctor: local facts that need no daemon.
type localStatus struct {
	Version string               `json:"version"`
	Paths   apiv0.Paths          `json:"paths"`
	Running bool                 `json:"running"`
	Daemon  *apiv0.Daemon        `json:"daemon,omitempty"`
	Ops     map[apiv0.State]int  `json:"operations,omitempty"`
	Store   *apiv0.StoreStatus   `json:"store,omitempty"`
	Network *apiv0.NetworkStatus `json:"network,omitempty"`
	// Error explains why the daemon could not be reached.
	Error *apiv0.Error `json:"error,omitempty"`
}

var cmdStatus = &command{
	name:    "status",
	summary: "one summary: daemon, version, paths, store, identity, addresses and connected peers",
	run: func(ctx context.Context, e *env, args []string) error {
		if err := parseFlags(e.flags(), args); err != nil {
			return err
		}
		r, err := e.openRepo()
		if err != nil {
			return err
		}
		ls := localStatus{Version: nu.Version, Paths: node.Paths(r)}
		if n, err := e.node(ctx); err != nil {
			ls.Error = toAPIError(err)
		} else if st, err := n.Status(ctx); err != nil {
			ls.Error = toAPIError(err)
		} else {
			ls.Running, ls.Daemon, ls.Ops, ls.Store, ls.Network, ls.Version = true, &st.Daemon, st.Operations, st.Store, st.Network, st.Version
		}
		if !ls.Running {
			// Read the store and identity in-process when nothing holds the repo lock.
			if n, release, err := e.local(r, false); err == nil {
				if st, err := n.Status(ctx); err == nil {
					ls.Store, ls.Network = st.Store, st.Network
				}
				release()
			}
		}
		return e.emit(ls, func(w io.Writer) {
			fmt.Fprintf(w, "%s %s\nrepo:       %s\n", nu.Name, ls.Version, ls.Paths.Root)
			if ls.Store != nil {
				fmt.Fprintf(w, "store:      %d objects, %s, %d pins\n", ls.Store.Objects, humanBytes(ls.Store.Bytes), ls.Store.Pins)
			}
			if ls.Network != nil {
				fmt.Fprintf(w, "peer id:    %s\n", ls.Network.PeerID)
				addrs := "none"
				if len(ls.Network.Addrs) > 0 {
					addrs = strings.Join(ls.Network.Addrs, "\n            ")
				}
				fmt.Fprintf(w, "addresses:  %s\n", addrs)
				if ls.Network.Serving {
					fmt.Fprintf(w, "network:    serving, %d peers connected\n", ls.Network.Peers)
					for _, p := range ls.Network.Connected {
						fmt.Fprintf(w, "            %s\n", p)
					}
				} else {
					fmt.Fprintf(w, "network:    not serving (shared content is available only while a holder is online; run `%s daemon` or `%s share`)\n", nu.Name, nu.Name)
				}
			}
			if !ls.Running {
				fmt.Fprintf(w, "daemon:     not running\n")
				if ls.Error != nil && ls.Error.Action != "" {
					fmt.Fprintf(w, "            → %s\n", ls.Error.Action)
				}
				return
			}
			fmt.Fprintf(w, "daemon:     running (pid %d, since %s)\n", ls.Daemon.PID, ls.Daemon.Started.Local().Format("15:04:05"))
			fmt.Fprintf(w, "api:        http://%s%s\ngateway:    http://%s/\n", ls.Daemon.API, apiv0.Prefix, ls.Daemon.Gateway)
			fmt.Fprintf(w, "operations: %s\n", formatCounts(ls.Ops))
		})
	},
}

func formatCounts(m map[apiv0.State]int) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, s := range []apiv0.State{apiv0.StateRunning, apiv0.StateSucceeded, apiv0.StateFailed, apiv0.StateCancelled} {
		if n := m[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return strings.Join(parts, ", ")
}

// check is one doctor finding.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
}

var cmdDoctor = &command{
	name:    "doctor",
	summary: "check the local setup and say what to do about problems",
	run: func(ctx context.Context, e *env, args []string) error {
		if err := parseFlags(e.flags(), args); err != nil {
			return err
		}
		var checks []check
		add := func(name, status, detail, action string) {
			checks = append(checks, check{Name: name, Status: status, Detail: detail, Action: action})
		}
		r, err := e.openRepo()
		if err != nil {
			add("repo", "fail", err.Error(), toAPIError(err).Action)
		} else {
			add("repo", "ok", r.Root(), "")
			if f, err := os.CreateTemp(r.Root(), ".doctor-*"); err != nil {
				add("repo writable", "fail", err.Error(), "fix the directory permissions or point "+repo.EnvPath+" elsewhere")
			} else {
				f.Close()
				os.Remove(f.Name())
				add("repo writable", "ok", "", "")
			}
			if cfg, err := r.Config(); err != nil {
				add("config", "fail", err.Error(), "fix the TOML syntax in "+r.Path(repo.ConfigFile))
			} else {
				add("config", "ok", "api "+cfg.API.Listen+", gateway "+cfg.Gateway.Listen, "")
			}
			if pid, _ := r.LockHolder(); pid > 0 {
				add("daemon lock", "ok", fmt.Sprintf("held by pid %d", pid), "")
			} else {
				add("daemon lock", "warn", "no daemon holds the lock", "start one with `"+nu.Name+" daemon`")
			}
			if n, err := e.node(ctx); err != nil {
				ae := toAPIError(err)
				add("daemon", "warn", ae.Message, ae.Action)
			} else if st, err := n.Status(ctx); err != nil {
				ae := toAPIError(err)
				add("daemon", "fail", ae.Message, "the address file is stale or the daemon is unhealthy; restart it with `"+nu.Name+" daemon`")
			} else {
				add("daemon", "ok", fmt.Sprintf("version %s, pid %d, api %s, gateway %s", st.Version, st.Daemon.PID, st.Daemon.API, st.Daemon.Gateway), "")
			}
		}
		add("network", "warn", "connectivity checks are not available in this build", "")
		failed := false
		for _, c := range checks {
			if c.Status == "fail" {
				failed = true
			}
		}
		err = e.emit(checks, func(w io.Writer) {
			for _, c := range checks {
				fmt.Fprintf(w, "%-4s %s", c.Status, c.Name)
				if c.Detail != "" {
					fmt.Fprintf(w, ": %s", c.Detail)
				}
				fmt.Fprintln(w)
				if c.Action != "" {
					fmt.Fprintf(w, "     → %s\n", c.Action)
				}
			}
		})
		if err != nil {
			return err
		}
		if failed {
			return apiv0.Errorf(apiv0.CodeFailed, "some checks failed").WithAction("follow the actions above")
		}
		return nil
	},
}
