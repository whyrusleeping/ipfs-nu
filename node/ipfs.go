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
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/store"
)

type ipfsImportJob struct {
	Request   apiv0.IPFSImportRequest `json:"request"`
	Operation apiv0.Operation         `json:"operation"`
}

func (n *Local) importDir() string           { return filepath.Join(n.Repo.Root(), "imports") }
func (n *Local) importFile(id string) string { return filepath.Join(n.importDir(), id+".json") }

// ImportIPFS starts or recovers a verified CAR import. The job descriptor is atomically
// persisted before work begins; a running job is restarted with its original operation ID
// after daemon restart. Converted blocks remain content-addressed in the store, so replay
// is safe and naturally reuses deterministic outputs.
func (n *Local) ImportIPFS(ctx context.Context, req apiv0.IPFSImportRequest) (*apiv0.Operation, error) {
	root, err := bridge.ParseIPFSCID(req.Root)
	if req.DryRun && strings.TrimSpace(req.CAR) == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dry-run requires a CAR source")
	}
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid IPFS root %q: %v", req.Root, err)
	}
	if strings.TrimSpace(req.CAR) != "" {
		if !filepath.IsAbs(req.CAR) {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR path must be absolute")
		}
		req.CAR, err = filepath.Abs(req.CAR)
		if err != nil {
			return nil, err
		}
		if _, err = os.Stat(req.CAR); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "cannot read CAR file %s: %v", req.CAR, err)
		}
	}
	o := n.Ops.Start("import-ipfs", root.String())
	job := ipfsImportJob{Request: req, Operation: o.Snapshot()}
	if err := n.saveIPFSJob(job); err != nil {
		o.Fail(err)
		return nil, err
	}
	n.runIPFSJob(o, job)
	snap := o.Snapshot()
	return &snap, nil
}
func (n *Local) runIPFSJob(o *op.Operation, job ipfsImportJob) {
	n.work.Add(1)
	go func() {
		defer n.work.Done()
		res, err := n.executeIPFSImport(o, &job)
		o.Finish(res, err)
		job.Operation = o.Snapshot()
		_ = n.saveIPFSJob(job)
	}()
}
func (n *Local) executeIPFSImport(o *op.Operation, job *ipfsImportJob) (any, error) {
	root, err := bridge.ParseIPFSCID(job.Request.Root)
	if err != nil {
		return nil, err
	}
	stage := func(s string) { o.Stage(s); job.Operation = o.Snapshot(); _ = n.saveIPFSJob(*job) }
	var src bridge.BlockSource
	if job.Request.CAR != "" {
		stage("reading-car")
		f, err := os.Open(job.Request.CAR)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		car, err := bridge.NewCARSource(f)
		if err != nil {
			return nil, err
		}
		found := false
		for _, r := range car.Roots {
			if r == root {
				found = true
				break
			}
		}
		if !found {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR does not declare requested root %s", root.String())
		}
		src = car
		if job.Request.DryRun {
			return map[string]any{"root": root.String(), "car": job.Request.CAR, "blocks": car.Blocks(), "verified": true, "dry_run": true}, nil
		}
	} else {
		stage("discovering-bitswap-providers")
		var closeSource func()
		src, closeSource, err = n.newBitswapSource(o.Context())
		if err != nil {
			return nil, err
		}
		defer closeSource()
	}
	stage("converting")
	sourceKind := "car"
	if job.Request.CAR == "" {
		sourceKind = "bitswap"
	}
	lastProgress := time.Now()
	res, err := bridge.Import(o.Context(), root, src, n.Store, n.Store, bridge.ImportOptions{ImporterVersion: "nu-" + nu.Version, Source: sourceKind, OnBlock: func(done int, _ bridge.IPFSCID) {
		if time.Since(lastProgress) < 100*time.Millisecond && done != 1 {
			return
		}
		lastProgress = time.Now()
		o.Progress(int64(done), 0, "blocks")
		job.Operation = o.Snapshot()
		_ = n.saveIPFSJob(*job)
	}})
	if err != nil {
		return nil, err
	}
	stage("pinning")
	p := store.Pin{CID: res.Root, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: "ipfs-import:" + root.String()}
	if err = n.Store.Pin(p); err != nil {
		return nil, err
	}
	manifestPin := store.Pin{CID: res.Manifest, Mode: store.Direct, Source: store.SourceExplicit, Name: "ipfs-manifest:" + root.String()}
	if err = n.Store.Pin(manifestPin); err != nil {
		return nil, err
	}
	o.Progress(int64(res.Bytes), int64(res.Bytes), "bytes")
	return res, nil
}
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
