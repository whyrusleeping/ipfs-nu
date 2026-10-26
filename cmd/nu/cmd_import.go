package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var cmdImport = &command{name: "import", summary: "verified, one-way content migration", sub: []*command{cmdImportIPFS, cmdImportCutover}}

var cmdImportIPFS = &command{name: "ipfs", args: "[--car <file>] [--dry-run] [--dataset <ds> [--cluster]] <ipfs-cid> | --pins <pins.json> [--car-dir <dir>] [--dry-run]",
	summary: "verify and convert IPFS UnixFS roots from CARv1 archives or the IPFS network (Bitswap/Amino and HTTP gateways)", run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		req := apiv0.IPFSImportRequest{}
		fs.StringVar(&req.CAR, "car", "", "CARv1 source (create with ipfs dag export <cid> > root.car)")
		fs.StringVar(&req.Pins, "pins", "", "Kubo pin inventory (ipfs pin ls --type recursive --enc=json > pins.json): import every recursive pin")
		fs.StringVar(&req.CARDir, "car-dir", "", "with --pins: per-root CARs named <cid>.car (default: retrieve over the network)")
		fs.BoolVar(&req.DryRun, "dry-run", false, "inventory and verify the structure without importing")
		fs.StringVar(&req.Dataset, "dataset", "", "archive dataset whose new head/version records this import's provenance manifest")
		clusterScope := fs.Bool("cluster", false, "the dataset is in the networked cluster (default: local)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if (req.Pins == "") == (fs.NArg() != 1) {
			return usagef("import ipfs needs one IPFS root CID, or --pins <inventory>")
		}
		req.Root = fs.Arg(0)
		if *clusterScope {
			req.Scope = node.ScopeCluster
		}
		for _, p := range []*string{&req.CAR, &req.Pins, &req.CARDir} {
			if *p != "" {
				abs, err := filepath.Abs(*p)
				if err != nil {
					return err
				}
				*p = abs
			}
		}
		var n node.Node
		var err error
		if (req.CAR != "" || req.CARDir != "") && !*clusterScope {
			n, err = e.nodeOrLocalWrite(ctx)
		} else {
			n, err = e.nodeOrLocal(ctx)
		}
		if err != nil {
			return err
		}
		operation, err := n.ImportIPFS(ctx, req)
		if err != nil {
			return err
		}
		if req.Pins != "" {
			return finishOperation(e, ctx, n, operation, false, printPinset)
		}
		return finishOperation(e, ctx, n, operation, false, printIPFSImport)
	}}

func printItems(w io.Writer, label string, items []apiv0.IPFSItem) {
	for _, it := range items {
		path := it.Path
		if path == "" {
			path = "."
		}
		fmt.Fprintf(w, "  %s %s: %s\n", label, path, it.Reason)
	}
}

func printIPFSImport(w io.Writer, r apiv0.IPFSImportResult) {
	if r.DryRun {
		complete := r.Complete == nil || *r.Complete
		fmt.Fprintf(w, "dry run of IPFS %s from %s: %d files, %d directories, %d symlinks, %d bytes (%d structure blocks verified)\n",
			r.IPFSRoot, r.Source, r.Files, r.Directories, r.Symlinks, r.Bytes, r.StructureBlocks)
		if r.Root != "" {
			fmt.Fprintf(w, "planned Nu root %s, manifest %s\n", r.Root, r.Manifest)
		}
		fmt.Fprintf(w, "estimated blob storage %d bytes (an estimate: excludes database overhead, may overcount shared blobs); headroom %d bytes\n", r.EstimatedPhysicalBytes, r.StoreHeadroom)
		if r.BlobEstimateFitsHeadroom != nil && !*r.BlobEstimateFitsHeadroom {
			fmt.Fprintln(w, "the estimate exceeds the store's headroom: free space or raise store.limit before importing")
		}
		if !complete {
			fmt.Fprintln(w, "inventory incomplete: some structure could not be retrieved; counts cover what was reached")
		}
		printItems(w, "missing", r.Missing)
		printItems(w, "unsupported", r.Unsupported)
		printItems(w, "transformed", r.Transformations)
		return
	}
	fmt.Fprintf(w, "imported IPFS %s as %s (%d files, %d bytes)\nmanifest %s\n", r.IPFSRoot, r.Root, r.Files, r.Bytes, r.Manifest)
	printItems(w, "transformed", r.Transformations)
	if r.Dataset != nil && r.Dataset.Version != nil {
		fmt.Fprintf(w, "dataset %s version %s records the import; check cutover with `nu import cutover %s`\n", r.Dataset.Name, r.Dataset.Version.TID, r.Dataset.Name)
	}
}

func printPinset(w io.Writer, r apiv0.IPFSPinsetResult) {
	for _, rr := range r.Roots {
		line := fmt.Sprintf("%-11s %s", rr.Status, rr.IPFSRoot)
		if rr.Name != "" {
			line += " (" + rr.Name + ")"
		}
		if rr.Root != "" {
			line += " → " + rr.Root
		}
		if rr.Error != "" {
			line += ": " + rr.Error
		}
		fmt.Fprintln(w, line)
		printItems(w, "missing", rr.Missing)
		printItems(w, "unsupported", rr.Unsupported)
		printItems(w, "transformed", rr.Transformations)
	}
	var counts []string
	for _, s := range []string{apiv0.IPFSImported, apiv0.IPFSPlanned, apiv0.IPFSMissing, apiv0.IPFSUnsupported, apiv0.IPFSFailed, apiv0.IPFSSkipped} {
		if r.Counts[s] > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", r.Counts[s], s))
		}
	}
	fmt.Fprintf(w, "%d pins: %s; %d bytes\n", r.Pins, strings.Join(counts, ", "), r.Bytes)
	if r.DryRun {
		fmt.Fprintf(w, "estimated blob storage %d bytes; headroom %d bytes\n", r.EstimatedPhysicalBytes, r.StoreHeadroom)
	}
}

var cmdImportCutover = &command{name: "cutover", args: "<dataset> [--cluster] [--timeout 10m]",
	summary: "check an imported dataset with a fresh client and its exact replication policy before replacing the IPFS source", run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		clusterScope := fs.Bool("cluster", false, "the dataset is in the networked cluster (default: local)")
		timeout := fs.String("timeout", "", "readiness wait bound (default 10m; 0 waits indefinitely)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("import cutover needs one dataset name")
		}
		req := apiv0.ImportCutoverRequest{Dataset: fs.Arg(0), Timeout: *timeout}
		if *clusterScope {
			req.Scope = node.ScopeCluster
		}
		n, err := e.nodeOrLocal(ctx)
		if err != nil {
			return err
		}
		res, err := n.ImportCutover(ctx, req)
		if err != nil {
			return err
		}
		if err := e.emit(res, func(w io.Writer) {
			fmt.Fprintf(w, "dataset %s head %s (version %s)\n", res.Dataset, res.Target, res.Version)
			if res.Imported != nil {
				fmt.Fprintf(w, "imported from %s, manifest %s\n", res.Imported.Source, res.Imported.Manifest)
			}
			if r := res.Retrieval; r != nil && r.OK {
				fmt.Fprintf(w, "fresh client %s retrieved %d files, %d bytes over the network\n", r.Client, r.Files, r.Bytes)
			}
			if r := res.Readiness; r != nil && r.Satisfied {
				fmt.Fprintf(w, "policy %s ready: %d of %d copies confirmed\n", r.Policy, len(r.Confirmed), r.Required)
			}
			if res.Recommend {
				fmt.Fprintln(w, "cutover recommended: readers and writers may move to Nu for this dataset")
			} else {
				fmt.Fprintln(w, "cutover NOT recommended yet:")
				for _, m := range res.Missing {
					fmt.Fprintln(w, "  - "+m)
				}
			}
			for _, note := range res.Notes {
				fmt.Fprintln(w, "note: "+note)
			}
		}); err != nil {
			return err
		}
		if !res.Recommend {
			return apiv0.Errorf(apiv0.CodeFailed, "cutover of %s is not recommended yet", res.Dataset).WithAction(strings.Join(res.Missing, "; "))
		}
		return nil
	}}
