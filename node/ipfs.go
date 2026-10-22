package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/node/op"
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
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "invalid IPFS root %q: %v", req.Root, err)
	}
	if strings.TrimSpace(req.CAR) == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "CAR path is required").WithAction("export a root with `ipfs dag export <cid> > root.car`")
	}
	req.CAR, err = filepath.Abs(req.CAR)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(req.CAR); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "cannot read CAR file %s: %v", req.CAR, err)
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
	stage("reading-car")
	f, err := os.Open(job.Request.CAR)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := bridge.NewCARSource(f)
	if err != nil {
		return nil, err
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
	if job.Request.DryRun {
		return map[string]any{"root": root.String(), "car": job.Request.CAR, "blocks": src.Blocks(), "verified": true, "dry_run": true}, nil
	}
	stage("converting")
	res, err := bridge.Import(o.Context(), root, src, n.Store, n.Store, bridge.ImportOptions{ImporterVersion: "nu-" + nu.Version, Source: "car"})
	if err != nil {
		return nil, err
	}
	stage("pinning")
	p := store.Pin{CID: res.Root, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: "ipfs-import:" + root.String()}
	if err = n.Store.Pin(p); err != nil {
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
