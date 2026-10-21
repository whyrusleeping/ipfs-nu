package node

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/target"
)

// Local is the in-process Node: the daemon serves it over the API, and daemonless
// commands will run it directly. Store-dependent methods are stubs until the store lands.
type Local struct {
	Repo *repo.Repo
	Ops  *op.Manager

	started time.Time
	// API and Gateway are the bound listen addresses, set by the daemon.
	API, Gateway string
}

// NewLocal creates a node on an opened repo with a fresh operation manager.
func NewLocal(r *repo.Repo) *Local {
	return &Local{Repo: r, Ops: op.NewManager(), started: time.Now()}
}

var _ Node = (*Local)(nil)

// Paths returns the repo layout.
func Paths(r *repo.Repo) apiv0.Paths {
	return apiv0.Paths{
		Root:    r.Root(),
		Config:  r.Path(repo.ConfigFile),
		Key:     r.Path(repo.KeyFile),
		DB:      r.Path(repo.DBFile),
		Blobs:   r.Path(repo.BlobDir),
		APIFile: r.Path(repo.APIFile),
	}
}

func (n *Local) Status(ctx context.Context) (*apiv0.Status, error) {
	return &apiv0.Status{
		Version: nu.Version,
		Daemon: apiv0.Daemon{
			PID:     os.Getpid(),
			Started: n.started,
			API:     n.API,
			Gateway: n.Gateway,
		},
		Paths:      Paths(n.Repo),
		Operations: n.Ops.Counts(),
	}, nil
}

func (n *Local) Operations(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Operation], error) {
	return apiv0.PageSlice(n.Ops.List(), cursor, limit, n.Ops.Epoch())
}

func (n *Local) op(id string) (*op.Operation, error) {
	o, ok := n.Ops.Get(id)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no operation %q", id).
			WithAction("list operations with `" + nu.Name + " operations`; finished operations are kept for a limited time")
	}
	return o, nil
}

func (n *Local) Operation(ctx context.Context, id string) (*apiv0.Operation, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	s := o.Snapshot()
	return &s, nil
}

func (n *Local) OperationEvents(ctx context.Context, id string, since int64) (EventStream, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	return o.Events(since), nil
}

func (n *Local) WaitOperation(ctx context.Context, id string, timeout time.Duration) (*apiv0.Operation, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	s, err := o.Wait(ctx, timeout)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (n *Local) CancelOperation(ctx context.Context, id string) error {
	o, err := n.op(id)
	if err != nil {
		return err
	}
	if o.Snapshot().Done() {
		return apiv0.Errorf(apiv0.CodeConflict, "operation %q has already finished", id)
	}
	o.Cancel()
	return nil
}

// Store-dependent methods. Each validates its input so callers get the same errors now as
// after the store lands, then reports not_implemented.

func (n *Local) Add(ctx context.Context, req apiv0.AddRequest) (*apiv0.Operation, error) {
	if req.Path == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "add needs a path").WithAction("give a file or directory path")
	}
	return nil, apiv0.NotImplemented("add")
}

func (n *Local) Cat(ctx context.Context, t string) (io.ReadCloser, error) {
	if err := checkCIDTarget(t); err != nil {
		return nil, err
	}
	return nil, apiv0.NotImplemented("cat")
}

func (n *Local) Ls(ctx context.Context, t string) (*apiv0.Listing, error) {
	if err := checkCIDTarget(t); err != nil {
		return nil, err
	}
	return nil, apiv0.NotImplemented("ls")
}

func (n *Local) PinAdd(ctx context.Context, req apiv0.PinRequest) (*apiv0.Operation, error) {
	if !target.IsCID(req.CID) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", req.CID)
	}
	return nil, apiv0.NotImplemented("pin add")
}

func (n *Local) PinRm(ctx context.Context, cid string) error {
	if !target.IsCID(cid) {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", cid)
	}
	return apiv0.NotImplemented("pin rm")
}

func (n *Local) Pins(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Pin], error) {
	return nil, apiv0.NotImplemented("pin ls")
}

func (n *Local) DagGet(ctx context.Context, t string) (json.RawMessage, error) {
	if err := checkCIDTarget(t); err != nil {
		return nil, err
	}
	return nil, apiv0.NotImplemented("dag get")
}

func (n *Local) DagPut(ctx context.Context, doc io.Reader) (*apiv0.DagPutResult, error) {
	return nil, apiv0.NotImplemented("dag put")
}

// checkCIDTarget parses a target and requires a form this build supports.
func checkCIDTarget(s string) error {
	t, err := target.Parse(s)
	if err != nil {
		return err
	}
	return t.Supported()
}
