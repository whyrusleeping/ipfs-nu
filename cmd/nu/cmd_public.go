package main

import (
	"context"
	"fmt"
	"io"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/target"
)

// `nu public` marks roots for the public HTTPS listener (backlog 0043; config [public]).
// Marking is explicit per root (operations.md P2) and the mark is a pin.

func publicNode(n node.Node) (node.PublicNode, error) {
	p, ok := n.(node.PublicNode)
	if !ok {
		return nil, apiv0.NotImplemented("public roots")
	}
	return p, nil
}

func printPublicRoot(w io.Writer, r apiv0.PublicRoot) {
	fmt.Fprintf(w, "%s", r.CID)
	if r.HTTPS != "" {
		fmt.Fprintf(w, "\t%s", r.HTTPS)
	}
	fmt.Fprintln(w)
}

var cmdPublic = &command{
	name:    "public",
	summary: "choose which held roots the public HTTPS seed serves (config [public])",
	sub: []*command{
		{
			name:    "add",
			args:    "<cid>",
			summary: "serve a held root on the public listener (also pins it)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || !target.IsCID(fs.Arg(0)) {
					return usagef("public add needs one CID")
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				p, err := publicNode(n)
				if err != nil {
					return err
				}
				r, err := p.PublicAdd(ctx, apiv0.PublicRequest{CID: fs.Arg(0)})
				if err != nil {
					return err
				}
				return e.emit(r, func(w io.Writer) {
					fmt.Fprintf(w, "public %s\n", r.CID)
					if r.HTTPS == "" {
						fmt.Fprintf(w, "served once a daemon with public.listen and public.domain runs (`%s config set public.listen :8443`)\n", nu.Name)
						return
					}
					fmt.Fprintf(w, "%s\n", r.HTTPS)
					if r.Site != "" {
						fmt.Fprintf(w, "%s\n", r.Site)
					}
					fmt.Fprintf(w, "%s\n", r.RASL)
				})
			},
		},
		{
			name:    "rm",
			args:    "<cid>",
			summary: "stop serving a root publicly",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				if fs.NArg() != 1 || !target.IsCID(fs.Arg(0)) {
					return usagef("public rm needs one CID")
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				p, err := publicNode(n)
				if err != nil {
					return err
				}
				if err := p.PublicRm(ctx, fs.Arg(0)); err != nil {
					return err
				}
				return e.emit(apiv0.PublicRoot{CID: fs.Arg(0)}, func(w io.Writer) { fmt.Fprintf(w, "no longer public: %s\n", fs.Arg(0)) })
			},
		},
		{
			name:    "ls",
			summary: "list public roots (with their URLs while the listener runs)",
			run: func(ctx context.Context, e *env, args []string) error {
				fs := e.flags()
				if err := parseFlags(fs, args); err != nil {
					return err
				}
				n, err := e.nodeOrLocalWrite(ctx)
				if err != nil {
					return err
				}
				p, err := publicNode(n)
				if err != nil {
					return err
				}
				return listAll(e, func(cursor string) (*apiv0.Page[apiv0.PublicRoot], error) {
					return p.PublicList(ctx, cursor, 0)
				}, printPublicRoot)
			},
		},
	},
}
