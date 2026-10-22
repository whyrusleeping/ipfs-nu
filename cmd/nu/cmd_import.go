package main

import (
	"context"
	"fmt"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"io"
	"path/filepath"
)

var cmdImport = &command{name: "import", summary: "verified, one-way content migration", sub: []*command{cmdImportIPFS}}
var cmdImportIPFS = &command{name: "ipfs", args: "[--car <file>] [--dry-run] <ipfs-cid>", summary: "verify and convert one IPFS UnixFS root from a CARv1 archive", run: func(ctx context.Context, e *env, args []string) error {
	fs := e.flags()
	req := apiv0.IPFSImportRequest{}
	fs.StringVar(&req.CAR, "car", "", "CARv1 source (create with ipfs dag export <cid> > root.car)")
	fs.BoolVar(&req.DryRun, "dry-run", false, "verify and inventory the CAR without importing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("import ipfs needs one IPFS root CID")
	}
	if req.DryRun && req.CAR == "" {
		return usagef("--dry-run requires --car <file>")
	}
	req.Root = fs.Arg(0)
	if req.CAR != "" {
		p, err := filepath.Abs(req.CAR)
		if err != nil {
			return err
		}
		req.CAR = p
	}
	var n node.Node
	var err error
	if req.CAR == "" {
		n, err = e.nodeOrLocal(ctx)
	} else {
		n, err = e.nodeOrLocalWrite(ctx)
	}
	if err != nil {
		return err
	}
	operation, err := n.ImportIPFS(ctx, req)
	if err != nil {
		return err
	}
	return finishOperation(e, ctx, n, operation, false, func(w io.Writer, r apiv0.IPFSImportResult) {
		if r.DryRun {
			fmt.Fprintf(w, "verified IPFS %s; planned Nu root %s; %d files, %d bytes; estimated blob storage %d bytes; headroom %d bytes\n", r.IPFSRoot, r.Root, r.Files, r.Bytes, r.EstimatedPhysicalBytes, r.StoreHeadroom)
			return
		}
		fmt.Fprintf(w, "imported IPFS %s as %s\nmanifest %s\n", r.IPFSRoot, r.Root, r.Manifest)
	})
}}
