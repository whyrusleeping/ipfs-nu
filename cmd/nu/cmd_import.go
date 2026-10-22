package main

import (
	"context"
	"fmt"
	"github.com/whyrusleeping/nu/api/apiv0"
	"io"
	"path/filepath"
)

var cmdImport = &command{name: "import", summary: "verified, one-way content migration", sub: []*command{cmdImportIPFS}}
var cmdImportIPFS = &command{name: "ipfs", args: "--car <file> [--dry-run] <ipfs-cid>", summary: "verify and convert one IPFS UnixFS root from a CARv1 archive", run: func(ctx context.Context, e *env, args []string) error {
	fs := e.flags()
	req := apiv0.IPFSImportRequest{}
	fs.StringVar(&req.CAR, "car", "", "CARv1 source (create with ipfs dag export <cid> > root.car)")
	fs.BoolVar(&req.DryRun, "dry-run", false, "verify and inventory the CAR without importing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || req.CAR == "" {
		return usagef("import ipfs needs --car <file> and one IPFS root CID")
	}
	req.Root = fs.Arg(0)
	p, err := filepath.Abs(req.CAR)
	if err != nil {
		return err
	}
	req.CAR = p
	n, err := e.nodeOrLocalWrite(ctx)
	if err != nil {
		return err
	}
	operation, err := n.ImportIPFS(ctx, req)
	if err != nil {
		return err
	}
	return finishOperation(e, ctx, n, operation, false, func(w io.Writer, r apiv0.IPFSImportResult) {
		fmt.Fprintf(w, "imported IPFS %s as %s\nmanifest %s\n", r.IPFSRoot, r.Root, r.Manifest)
	})
}}
