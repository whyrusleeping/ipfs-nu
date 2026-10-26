package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/store"
)

type ipfsImportJob struct {
	Request   apiv0.IPFSImportRequest `json:"request"`
	Operation apiv0.Operation         `json:"operation"`
	// Pins is the parsed inventory of a pinset job; Done the reports of its first
	// len(Done) pins, persisted after each so a resumed job continues with the next.
	Pins []bridge.Pin           `json:"pins,omitempty"`
	Done []apiv0.IPFSRootReport `json:"done,omitempty"`
}

func (n *Local) importDir() string           { return filepath.Join(n.Repo.Root(), "imports") }
func (n *Local) importFile(id string) string { return filepath.Join(n.importDir(), id+".json") }

// ipfsSourceHook, when set (tests), wraps each import's upstream block source beneath its
// verified checkpoints, so a test can observe or stall original-block requests.
var ipfsSourceHook func(bridge.BlockSource) bridge.BlockSource

// ImportIPFS starts or recovers a verified IPFS import: one root from a CAR or the
// network, or a pinset. The job descriptor is atomically persisted before work begins; a
// running job is restarted with its original operation ID after daemon restart. Converted
// blocks remain content-addressed in the store, so replay is safe and naturally reuses
// deterministic outputs.
func (n *Local) ImportIPFS(ctx context.Context, req apiv0.IPFSImportRequest) (*apiv0.Operation, error) {
	job := ipfsImportJob{Request: req}
	target, err := n.prepareIPFSImport(&job)
	if err != nil {
		return nil, err
	}
	o := n.Ops.Start("import-ipfs", target)
	job.Operation = o.Snapshot()
	if err := n.saveIPFSJob(job); err != nil {
		o.Fail(err)
		return nil, err
	}
	n.runIPFSJob(o, job)
	snap := o.Snapshot()
	return &snap, nil
}

func absFile(what, p string, dir bool) (string, error) {
	if !filepath.IsAbs(p) {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "%s path must be absolute", what)
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil {
		return "", apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s %s: %v", what, p, err)
	}
	if st.IsDir() != dir {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "%s %s is not a %s", what, p, map[bool]string{true: "directory", false: "file"}[dir])
	}
	return p, nil
}

// prepareIPFSImport validates a request and returns the operation target.
func (n *Local) prepareIPFSImport(job *ipfsImportJob) (string, error) {
	req := &job.Request
	var err error
	if req.CARDir != "" {
		if req.CARDir, err = absFile("CAR directory", req.CARDir, true); err != nil {
			return "", err
		}
	}
	if req.CAR != "" {
		if req.CAR, err = absFile("CAR file", req.CAR, false); err != nil {
			return "", err
		}
	}
	if req.Pins != "" {
		if req.Root != "" || req.CAR != "" {
			return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "a pinset import takes its roots from the inventory; drop the root/car").
				WithAction("give per-root CARs as <car_dir>/<cid>.car, or omit car_dir to retrieve over the network")
		}
		if req.Dataset != "" {
			return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "a pinset has many roots and a dataset version has one").
				WithAction("import each root into its own dataset with `" + nu.Name + " import ipfs <cid> --dataset <ds>`")
		}
		if req.Pins, err = absFile("pin inventory", req.Pins, false); err != nil {
			return "", err
		}
		f, err := os.Open(req.Pins)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if job.Pins, err = bridge.ParsePins(f); err != nil {
			return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).
				WithAction("create it read-only with `ipfs pin ls --type recursive --enc=json > pins.json`")
		}
		return req.Pins, nil
	}
	if req.CARDir != "" {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "car_dir applies to pinset imports; give one root's CAR as car")
	}
	root, err := bridge.ParseIPFSCID(req.Root)
	if err != nil {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid IPFS root %q: %v", req.Root, err)
	}
	if req.Dataset != "" {
		if req.Scope == "" {
			req.Scope = ScopeLocal
		}
		if _, _, err := n.importDataset(req.Scope, req.Dataset); err != nil {
			return "", err
		}
	}
	return root.String(), nil
}

// importDataset finds a dataset an import may write: it must keep version history, since
// the signed `imported` reference lives in the version record.
func (n *Local) importDataset(scope, name string) (*cluster.Cluster, *cluster.Dataset, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, nil, err
	}
	d, ok := c.View().Dataset(name)
	if !ok {
		return nil, nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope).
			WithAction("create it with `" + nu.Name + " dataset create " + name + " --archive`")
	}
	if d.Collection != nil {
		return nil, nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is a collection: its heads come from item pushes", name)
	}
	if d.Policy.History == nil {
		return nil, nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dataset %q keeps no version history, so it cannot carry the signed import reference", name).
			WithAction("import into a dataset created with `" + nu.Name + " dataset create <ds> --archive`")
	}
	return c, d, nil
}

func (n *Local) runIPFSJob(o *op.Operation, job ipfsImportJob) {
	n.work.Add(1)
	go func() {
		defer n.work.Done()
		res, clean, err := n.executeIPFSImport(o, &job)
		o.Finish(res, err)
		if err != nil && n.Ops.ShuttingDown() && o.Context().Err() != nil {
			return // keep the job running on disk: the next start resumes it
		}
		job.Operation = o.Snapshot()
		_ = n.saveIPFSJob(job)
		if clean && err == nil {
			// Every root is converted and pinned: the verified source blocks are spent.
			_ = os.RemoveAll(filepath.Join(n.importDir(), job.Operation.ID))
		}
	}()
}

// ipfsSource opens one root's source: a CAR (whose declared roots must include root) or
// the network (newNetworkSource). blocks is the CAR's block count (0 otherwise).
func (n *Local) ipfsSource(ctx context.Context, carPath string, root bridge.IPFSCID, dryRun bool) (src bridge.BlockSource, kind string, blocks int, closeFn func(), err error) {
	if carPath == "" {
		src, kind, closeFn, err = n.newNetworkSource(ctx, dryRun)
		return src, kind, 0, closeFn, err
	}
	f, err := os.Open(carPath)
	if err != nil {
		return nil, "", 0, nil, err
	}
	defer f.Close()
	car, err := bridge.NewCARSource(f)
	if err != nil {
		return nil, "", 0, nil, err
	}
	for _, r := range car.Roots {
		if r == root {
			return car, "car", car.Blocks(), func() { _ = car.Close() }, nil
		}
	}
	_ = car.Close()
	return nil, "", 0, nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR %s does not declare requested root %s", carPath, root.String())
}

func (n *Local) executeIPFSImport(o *op.Operation, job *ipfsImportJob) (any, bool, error) {
	if job.Pins != nil {
		return n.importPinset(o, job)
	}
	ctx := o.Context()
	root, err := bridge.ParseIPFSCID(job.Request.Root)
	if err != nil {
		return nil, false, err
	}
	stage := func(s string) { o.Stage(s); job.Operation = o.Snapshot(); _ = n.saveIPFSJob(*job) }
	if job.Request.CAR != "" {
		stage("reading-car")
	} else {
		stage("discovering-providers")
	}
	src, kind, blocks, closeSrc, err := n.ipfsSource(ctx, job.Request.CAR, root, job.Request.DryRun)
	if err != nil {
		return nil, false, err
	}
	defer closeSrc()
	if job.Request.DryRun {
		stage("inventory")
		out, err := n.ipfsDryRun(ctx, root, src, kind)
		if err != nil {
			return nil, false, err
		}
		out.Blocks = blocks
		return out, false, nil
	}
	res, err := n.importIPFSRoot(o, job, root, src, kind)
	if err != nil {
		return nil, false, err
	}
	out := importResult(res, kind)
	if job.Request.Dataset != "" {
		stage("writing-dataset")
		if out.Dataset, err = n.importHead(ctx, job.Request.Scope, job.Request.Dataset, res); err != nil {
			return nil, false, err
		}
	}
	o.Progress(int64(res.Bytes), int64(res.Bytes), "bytes")
	return out, true, nil
}

func items(in []bridge.Item) []apiv0.IPFSItem {
	var out []apiv0.IPFSItem
	for _, x := range in {
		out = append(out, apiv0.IPFSItem{Path: x.Path, CID: x.CID, Reason: x.Reason})
	}
	return out
}

func importResult(res bridge.ImportResult, kind string) *apiv0.IPFSImportResult {
	return &apiv0.IPFSImportResult{IPFSRoot: res.IPFSRoot, Root: res.Root.String(), Manifest: res.Manifest.String(), Source: kind,
		Files: res.Files, Bytes: res.Bytes, EstimatedPhysicalBytes: res.EstimatedPhysicalBytes, Transformations: items(res.Transformations)}
}

// ipfsDryRun inventories root's structure without writing to the store. A CAR, being
// local, is also converted in memory to report the planned Nu root and manifest.
func (n *Local) ipfsDryRun(ctx context.Context, root bridge.IPFSCID, src bridge.BlockSource, kind string) (*apiv0.IPFSImportResult, error) {
	opts := bridge.ImportOptions{ImporterVersion: "nu-" + nu.Version, Source: kind}
	inv, err := bridge.TakeInventory(ctx, root, src, opts)
	if err != nil {
		return nil, err
	}
	out := &apiv0.IPFSImportResult{IPFSRoot: root.String(), Source: kind, DryRun: true, Files: inv.Files, Bytes: inv.Bytes,
		EstimatedPhysicalBytes: inv.EstimatedPhysicalBytes, Complete: &inv.Complete, StructureBlocks: inv.StructureBlocks,
		Directories: inv.Directories, Symlinks: inv.Symlinks, Missing: items(inv.Missing), Unsupported: items(inv.Unsupported),
		Transformations: items(inv.Transformations)}
	if kind == "car" && inv.Supported() {
		plan, err := bridge.Import(ctx, root, src, tree.HashSink{}, mst.MemStore{}, opts)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var missing *bridge.MissingBlockError
			if errors.As(err, &missing) {
				incomplete := false
				out.Complete = &incomplete
				out.Missing = append(out.Missing, apiv0.IPFSItem{CID: missing.CID.String(), Reason: missing.Err.Error()})
			} else {
				out.Unsupported = append(out.Unsupported, apiv0.IPFSItem{CID: root.String(), Reason: err.Error()})
			}
		} else {
			out.Root, out.Manifest, out.Bytes, out.EstimatedPhysicalBytes = plan.Root.String(), plan.Manifest.String(), plan.Bytes, plan.EstimatedPhysicalBytes
		}
	}
	if err := n.headroom(out.EstimatedPhysicalBytes, &out.StoreHeadroom, &out.BlobEstimateFitsHeadroom); err != nil {
		return nil, err
	}
	return out, nil
}

func (n *Local) headroom(estimate uint64, headroom *int64, fits **bool) error {
	stats, err := n.Store.Stats()
	if err != nil {
		return err
	}
	ok := stats.Headroom < 0 || estimate <= uint64(stats.Headroom)
	*headroom, *fits = stats.Headroom, &ok
	return nil
}

// importIPFSRoot converts one root through the operation's verified block checkpoints and
// pins the result and its manifest.
func (n *Local) importIPFSRoot(o *op.Operation, job *ipfsImportJob, root bridge.IPFSCID, src bridge.BlockSource, kind string) (bridge.ImportResult, error) {
	o.Stage("converting")
	job.Operation = o.Snapshot()
	_ = n.saveIPFSJob(*job)
	// Per-operation block checkpoints survive cancellation and daemon restart. The
	// wrapper verifies before atomic persistence and revalidates every cache hit.
	if ipfsSourceHook != nil {
		src = ipfsSourceHook(src)
	}
	src = bridge.CheckpointSource{Dir: filepath.Join(n.importDir(), o.Snapshot().ID, "blocks"), Upstream: src}
	lastProgress := time.Now()
	res, err := bridge.Import(o.Context(), root, src, n.Store, n.Store, bridge.ImportOptions{ImporterVersion: "nu-" + nu.Version, Source: kind, OnBlock: func(done int, _ bridge.IPFSCID) {
		if time.Since(lastProgress) < 100*time.Millisecond && done != 1 {
			return
		}
		lastProgress = time.Now()
		o.Progress(int64(done), 0, "blocks")
		job.Operation = o.Snapshot()
		_ = n.saveIPFSJob(*job)
	}})
	if err != nil {
		return res, err
	}
	o.Stage("pinning")
	p := store.Pin{CID: res.Root, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: "ipfs-import:" + root.String()}
	if err = n.Store.Pin(p); err != nil {
		return res, err
	}
	manifestPin := store.Pin{CID: res.Manifest, Mode: store.Direct, Source: store.SourceExplicit, Name: "ipfs-manifest:" + root.String()}
	return res, n.Store.Pin(manifestPin)
}

// importHead writes the imported root as the dataset's head and version, whose signed
// `imported` field references the provenance manifest. A replay after the write (a
// resumed job) finds the same head and reference and writes nothing.
func (n *Local) importHead(ctx context.Context, scope, name string, res bridge.ImportResult) (*apiv0.Dataset, error) {
	c, d, err := n.importDataset(scope, name)
	if err != nil {
		return nil, err
	}
	imported := &cluster.Imported{Source: "ipfs://" + res.IPFSRoot, Manifest: res.Manifest.String()}
	hs := histFor(c, name, d)
	if hs.head == res.Root.String() {
		for _, e := range hs.hist {
			if e.TID == hs.headTID && e.Imported != nil && *e.Imported == *imported {
				return n.datasetInfo(c, name), nil
			}
		}
	}
	kind, size, err := n.headKind(res.Root)
	if err != nil {
		return nil, err
	}
	return n.writeHeadImported(ctx, c, name, d, res.Root, kind, size, "imported from "+imported.Source, "", imported)
}

// importPinset imports (or inventories) every recursive pin of a Kubo pin inventory and
// returns the reconciliation report. One root's failure never discards the others.
func (n *Local) importPinset(o *op.Operation, job *ipfsImportJob) (any, bool, error) {
	ctx := o.Context()
	req := job.Request
	var network bridge.BlockSource
	var networkKind string
	defer func() {
		if c, ok := network.(interface{ Close() }); ok {
			c.Close()
		}
	}()
	for i := len(job.Done); i < len(job.Pins); i++ {
		pin := job.Pins[i]
		o.Log(fmt.Sprintf("root %d/%d: %s", i+1, len(job.Pins), pin.CID))
		rep := apiv0.IPFSRootReport{IPFSRoot: pin.CID, Name: pin.Name, PinType: pin.Type}
		root, err := bridge.ParseIPFSCID(pin.CID)
		switch {
		case pin.Type != "recursive":
			rep.Status, rep.Error = apiv0.IPFSSkipped, pin.Type+" pin: only recursive pins name complete DAGs to import"
		case err != nil:
			rep.Status, rep.Error = apiv0.IPFSUnsupported, err.Error()
		default:
			var src bridge.BlockSource
			kind, closeSrc := networkKind, func() {}
			if req.CARDir != "" {
				carPath := filepath.Join(req.CARDir, pin.CID+".car")
				if _, err = os.Stat(carPath); err != nil {
					rep.Status, rep.Error = apiv0.IPFSMissing, "no CAR at "+carPath+"; export it read-only with `ipfs dag export "+pin.CID+" > "+carPath+"`"
					break
				}
				src, kind, _, closeSrc, err = n.ipfsSource(ctx, carPath, root, req.DryRun)
			} else {
				if network == nil {
					var closeNet func()
					if network, networkKind, closeNet, err = n.newNetworkSource(ctx, req.DryRun); err == nil {
						network = closingSource{network, closeNet}
					}
				}
				kind = networkKind
				src = network
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil, false, ctx.Err()
				}
				rep.Status, rep.Error = apiv0.IPFSFailed, err.Error()
				break
			}
			err = n.importPinRoot(o, job, root, src, kind, &rep)
			closeSrc()
			if err != nil {
				return nil, false, err
			}
		}
		job.Done = append(job.Done, rep)
		job.Operation = o.Snapshot()
		if err := n.saveIPFSJob(*job); err != nil {
			return nil, false, err
		}
	}
	out := &apiv0.IPFSPinsetResult{Inventory: req.Pins, Pins: len(job.Pins), DryRun: req.DryRun, Counts: map[string]int{}, Roots: job.Done}
	for _, r := range job.Done {
		out.Counts[r.Status]++
		out.Bytes += r.Bytes
	}
	if req.DryRun {
		for _, r := range job.Done {
			out.EstimatedPhysicalBytes += r.EstimatedPhysicalBytes
		}
		if err := n.headroom(out.EstimatedPhysicalBytes, &out.StoreHeadroom, &out.BlobEstimateFitsHeadroom); err != nil {
			return nil, false, err
		}
	}
	return out, out.Counts[apiv0.IPFSImported] == len(job.Done), nil
}

// importPinRoot fills one pinset report entry; only cancellation is returned as an error.
func (n *Local) importPinRoot(o *op.Operation, job *ipfsImportJob, root bridge.IPFSCID, src bridge.BlockSource, kind string, rep *apiv0.IPFSRootReport) error {
	ctx := o.Context()
	if job.Request.DryRun {
		out, err := n.ipfsDryRun(ctx, root, src, kind)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rep.Status, rep.Error = apiv0.IPFSFailed, err.Error()
			return nil
		}
		rep.Root, rep.Manifest, rep.Files, rep.Bytes, rep.EstimatedPhysicalBytes = out.Root, out.Manifest, out.Files, out.Bytes, out.EstimatedPhysicalBytes
		rep.Missing, rep.Unsupported, rep.Transformations = out.Missing, out.Unsupported, out.Transformations
		switch {
		case len(out.Missing) > 0:
			rep.Status = apiv0.IPFSMissing
		case len(out.Unsupported) > 0:
			rep.Status = apiv0.IPFSUnsupported
		default:
			rep.Status = apiv0.IPFSPlanned
		}
		return nil
	}
	res, err := n.importIPFSRoot(o, job, root, src, kind)
	var missing *bridge.MissingBlockError
	switch {
	case err == nil:
		rep.Status, rep.Root, rep.Manifest, rep.Files, rep.Bytes = apiv0.IPFSImported, res.Root.String(), res.Manifest.String(), res.Files, res.Bytes
		rep.Transformations = items(res.Transformations)
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &missing):
		rep.Status, rep.Error = apiv0.IPFSMissing, err.Error()
		rep.Missing = []apiv0.IPFSItem{{CID: missing.CID.String(), Reason: missing.Err.Error()}}
	case errors.Is(err, bridge.ErrDigestMismatch):
		rep.Status, rep.Error = apiv0.IPFSFailed, err.Error()
	default:
		rep.Status, rep.Error = apiv0.IPFSUnsupported, err.Error()
	}
	return nil
}

type closingSource struct {
	bridge.BlockSource
	close func()
}

func (c closingSource) Close() { c.close() }

func (n *Local) saveIPFSJob(j ipfsImportJob) error {
	if err := os.MkdirAll(n.importDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(n.importDir(), ".import-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, n.importFile(j.Operation.ID)); err != nil {
		return err
	}
	d, err := os.Open(n.importDir())
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}
func (n *Local) recoverIPFSImports() error {
	dir := n.importDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var job ipfsImportJob
		if err = json.Unmarshal(data, &job); err != nil {
			return fmt.Errorf("node: invalid IPFS import job %s: %w", entry.Name(), err)
		}
		id := job.Operation.ID
		if id == "" || strings.ContainsAny(id, "/\\") || filepath.Base(entry.Name()) != id+".json" {
			return fmt.Errorf("node: invalid IPFS import job identifier %q", id)
		}
		o, err := n.Ops.StartWithID(id, job.Operation.Kind, job.Operation.Target)
		if err != nil {
			return err
		}
		if job.Operation.Done() {
			if job.Operation.Error != nil {
				o.Finish(nil, job.Operation.Error)
			} else {
				o.Finish(json.RawMessage(job.Operation.Result), nil)
			}
			continue
		}
		job.Operation = o.Snapshot()
		n.runIPFSJob(o, job)
	}
	return nil
}

// IPFSRouting is the delegated-routing endpoint asked for HTTP trustless-gateway providers
// of IPFS blocks (IPNI, where Filecoin storage providers announce them); the config key
// ipfs.routing_url overrides it and "off" disables the gateway transport.
var IPFSRouting = "https://cid.contact"

// ipfsGatewayHook adjusts the gateway source (tests: a local routing endpoint).
var ipfsGatewayHook func(*bridge.GatewaySource)

// newGatewaySource returns the HTTP trustless-gateway source, or nil when it is disabled or
// this node is a private or isolated cluster member (D15: no public routing).
func (n *Local) newGatewaySource() *bridge.GatewaySource {
	if c, _ := n.netCluster(); c != nil && c.Mode() != cluster.ModePublic {
		return nil
	}
	routing := IPFSRouting
	if v, err := n.Repo.ConfigGet("ipfs.routing_url"); err == nil {
		if s, ok := v.(string); ok {
			routing = s
		}
	}
	if routing == "" || routing == "off" {
		return nil
	}
	src := bridge.NewGatewaySource(n.httpClient(), routing)
	if ipfsGatewayHook != nil {
		ipfsGatewayHook(src)
	}
	return src
}

// newNetworkSource races Bitswap (Amino provider discovery) and HTTP trustless gateways
// (delegated routing); the first verified block wins and its transport is asked first for
// the next block (bridge.FirstSource). Either one alone suffices. kind names the transports
// used ("bitswap+gateway", "bitswap", "gateway"). Dry runs do not read ahead: they skip raw
// leaves.
func (n *Local) newNetworkSource(ctx context.Context, dryRun bool) (bridge.BlockSource, string, func(), error) {
	gw := n.newGatewaySource()
	if gw != nil && dryRun {
		gw.Readahead = 0
	}
	bs, closeBS, err := n.newBitswapSource(ctx)
	switch {
	case err == nil && gw != nil:
		return bridge.NewFirstSource(bs, gw), "bitswap+gateway", func() { closeBS(); gw.Close() }, nil
	case err == nil:
		return bs, "bitswap", closeBS, nil
	case gw != nil && ctx.Err() == nil:
		return gw, "gateway", gw.Close, nil
	}
	if gw != nil {
		gw.Close()
	}
	return nil, "", nil, err
}

func (n *Local) newBitswapSource(ctx context.Context) (bridge.BlockSource, func(), error) {
	// Recovery may run before the daemon attaches its swarm; wait for that transition.
	var netStack ClusterNet
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for netStack == nil {
		n.clusters.mu.Lock()
		netStack = n.clusters.sw
		n.clusters.mu.Unlock()
		if netStack != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
	stack, ok := netStack.(interface{ Sessions() *host.Sessions })
	if !ok {
		return nil, nil, errors.New("IPFS Bitswap import requires a network exposing libp2p sessions")
	}
	h := netStack.Host()
	if finder, ok := netStack.(bridge.ProviderFinder); ok {
		src := bridge.NewBitswapSource(h, finder)
		return src, src.Close, nil
	}
	for _, protocol := range h.Protocols() {
		if protocol == kad.Protocol {
			return nil, nil, errors.New("amino provider discovery is already owned by another service")
		}
	}
	dht, err := kad.New(ctx, kad.Config{Host: h, Sessions: stack.Sessions()})
	if err != nil {
		return nil, nil, err
	}
	dns, err := lookup.NewDNS(lookup.DNSConfig{})
	if err != nil {
		_ = dht.Close()
		return nil, nil, err
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	bootstrapErr := dht.BootstrapDNS(bootstrapCtx, dns)
	cancel()
	if len(dht.Table().Peers()) == 0 {
		_ = dht.Close()
		if bootstrapErr != nil {
			return nil, nil, fmt.Errorf("amino bootstrap failed: %w", bootstrapErr)
		}
		return nil, nil, errors.New("amino returned no usable routing peers")
	}
	src := bridge.NewBitswapSource(h, dht)
	return src, func() { src.Close(); _ = dht.Close() }, nil
}
