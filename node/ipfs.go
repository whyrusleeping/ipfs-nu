package node

import (
	"context"
	"os"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/store"
)

// ImportIPFS starts an import from an explicitly supplied local CARv1 file. The CAR is
// read-only; Kubo repositories are not opened or modified.
func (n *Local) ImportIPFS(ctx context.Context, req apiv0.IPFSImportRequest) (*apiv0.Operation, error) {
	root, err := bridge.ParseIPFSCID(req.Root)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid IPFS root %q: %v", req.Root, err)
	}
	if strings.TrimSpace(req.CAR) == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR path is required").WithAction("export a root with `ipfs dag export <cid> > root.car`")
	}
	return n.start("import-ipfs", root.String(), func(o *op.Operation) (any, error) {
		o.Stage("reading-car")
		f, e := os.Open(req.CAR)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		src, e := bridge.NewCARSource(f)
		if e != nil {
			return nil, e
		}
		found := false
		for _, r := range src.Roots {
			if r == root {
				found = true
				break
			}
		}
		if !found {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR does not declare requested root %s", root.String())
		}
		if req.DryRun {
			return map[string]any{"root": root.String(), "car": req.CAR, "blocks": src.Blocks(), "verified": true, "dry_run": true}, nil
		}
		o.Stage("converting")
		res, e := bridge.Import(o.Context(), root, src, n.Store, n.Store, bridge.ImportOptions{ImporterVersion: "nu-" + nu.Version, Source: "car"})
		if e != nil {
			return nil, e
		}
		o.Stage("pinning")
		p := store.Pin{CID: res.Root, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: "ipfs-import:" + root.String()}
		if e = n.Store.Pin(p); e != nil {
			return nil, e
		}
		o.Progress(int64(res.Bytes), int64(res.Bytes), "bytes")
		return res, nil
	}), nil
}
