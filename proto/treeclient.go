package proto

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// TreeStore receives the verified items of a /nu/tree/1 response; resuming at a cursor
// reads the nodes on the cursor's path back from it. *store.Store implements it.
type TreeStore interface {
	Get(c cid.CID) ([]byte, error)
	Put(c cid.CID, data []byte) error
}

// TreeResult describes one /nu/tree/1 response.
type TreeResult struct {
	Nodes, Blobs int   // data frames verified and stored
	Bytes        int64 // their total size
	// Have lists items the server skipped as present in a have tree; Missing items it does
	// not hold. Neither is descended; fetch what the store lacks by other means.
	Have, Missing []cid.CID
	// More reports that the server stopped at its budget: request again with Cursor.
	More bool
	// Cursor is the position after the last item handled (the request's cursor if none).
	// After an error it resumes the walk at any peer holding the tree.
	Cursor *Cursor
	// Handled counts the answered CIDs of a nodes request (data or marker), in order.
	Handled int
}

// FetchTree sends one /nu/tree/1 request and stores every item as it verifies against the
// CID its already verified parent links. The result is meaningful after errors too: its
// Cursor resumes the walk. Errors wrapping ErrCorrupt mean the peer sent bad data or broke
// the framing; *StatusError are refusals; anything else is transport or local.
func FetchTree(ctx context.Context, open Opener, st TreeStore, req TreeRequest) (TreeResult, error) {
	res := TreeResult{Cursor: req.Cursor}
	b, err := req.Marshal()
	if err != nil {
		return res, err
	}
	c, err := start(ctx, open, TreeID, b)
	if err != nil {
		return res, err
	}
	var h bytesHeader
	if err := c.header(MaxBytesHeaderSize, &h, func() (Status, string) { return Status(h.Status), h.Error }); err != nil {
		return res, c.end(err)
	}
	res, err = readTree(c.r, st, &req)
	if err == nil {
		err = c.eof()
	}
	return res, c.end(err)
}

type treeRead struct {
	r   io.Reader
	st  TreeStore
	req *TreeRequest
	res TreeResult
}

func (t *treeRead) frame() (byte, []byte, error) {
	b, err := wire.ReadMessage(t.r, MaxTreeFrame)
	if errors.Is(err, wire.ErrLimit) {
		return 0, nil, corrupt("tree frame over %d bytes", MaxTreeFrame)
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return 0, nil, err
	}
	if len(b) == 0 {
		return 0, nil, corrupt("empty tree frame")
	}
	return b[0], b[1:], nil
}

func (t *treeRead) trailer(payload []byte) (bool, error) {
	var tr treeTrailer
	if err := (drisl.DecOptions{MaxSize: MaxBytesHeaderSize, MaxDepth: 4}).Unmarshal(payload, &tr); err != nil {
		return false, corrupt("tree trailer: %w", err)
	}
	return tr.More, nil
}

func (t *treeRead) visit(l *loc, c cid.CID, k itemKind, _ func() bool) ([]byte, error) {
	tag, payload, err := t.frame()
	if err != nil {
		return nil, err
	}
	done := func(after bool) {
		if l != nil {
			t.res.Cursor = l.cursor(after)
		}
		t.res.Handled++
	}
	switch tag {
	case frameEnd:
		more, err := t.trailer(payload)
		if err != nil {
			return nil, err
		}
		if !more {
			return nil, corrupt("tree response ended before the walk")
		}
		t.res.More = true
		return nil, errBudget
	case frameHave, frameMissing:
		if len(payload) != 0 {
			return nil, corrupt("payload on a tree marker frame")
		}
		if tag == frameHave {
			t.res.Have = append(t.res.Have, c)
		} else {
			t.res.Missing = append(t.res.Missing, c)
		}
		done(true)
		return nil, nil
	case frameData:
	default:
		return nil, corrupt("unknown tree frame tag %d", tag)
	}
	max := uint64(drisl.MaxNodeSize)
	if k == kindBlob {
		max = t.req.inline()
		if len(t.req.Nodes) > 0 {
			max = MaxInline
		}
	}
	if uint64(len(payload)) > max {
		return nil, corrupt("tree item of %d bytes over the %d-byte limit", len(payload), max)
	}
	if err := c.Verify(payload); err != nil {
		return nil, corrupt("tree item does not match %s", c)
	}
	if err := t.st.Put(c, payload); err != nil {
		return nil, err
	}
	if k == kindBlob {
		t.res.Blobs++
	} else {
		t.res.Nodes++
	}
	t.res.Bytes += int64(len(payload))
	done(k == kindBlob)
	return payload, nil
}

// readTree runs the client side of the walk over a response body.
func readTree(r *bufio.Reader, st TreeStore, req *TreeRequest) (TreeResult, error) {
	t := &treeRead{r: r, st: st, req: req, res: TreeResult{Cursor: req.Cursor}}
	w := newTreeWalk(req, st.Get)
	w.visit = t.visit
	err := w.run()
	if errors.Is(err, errBudget) {
		return t.res, nil
	}
	if err != nil {
		return t.res, err
	}
	tag, payload, err := t.frame()
	if err != nil {
		return t.res, err
	}
	if tag != frameEnd {
		return t.res, corrupt("tree frame after the end of the walk")
	}
	if more, err := t.trailer(payload); err != nil || more {
		return t.res, errors.Join(err, corrupt("budget trailer after the end of the walk"))
	}
	return t.res, nil
}
