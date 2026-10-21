package proto

import (
	"bufio"
	"errors"
	"io"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/core/varint"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Default /nu/tree/1 response budgets.
const (
	DefaultTreeFrames = 4096
	DefaultTreeBytes  = 16 << 20
)

// treeServe is one /nu/tree/1 response being written.
type treeServe struct {
	s       *Server
	w       *bufio.Writer
	inline  uint64
	limit   uint64
	started bool
	frames  uint64
	bytes   uint64
}

func (t *treeServe) start() error {
	if t.started {
		return nil
	}
	t.started = true
	b, err := drisl.Marshal(bytesHeader{Status: uint64(StatusOK)})
	if err != nil {
		return err
	}
	return wire.WriteMessage(t.w, b, MaxBytesHeaderSize)
}

func (t *treeServe) frame(tag byte, payload []byte) error {
	if err := t.start(); err != nil {
		return err
	}
	if _, err := t.w.Write(varint.Append(nil, uint64(1+len(payload)))); err != nil {
		return err
	}
	if err := t.w.WriteByte(tag); err != nil {
		return err
	}
	_, err := t.w.Write(payload)
	return err
}

// load returns an item's bytes, or nil if the server cannot send it.
func (t *treeServe) load(c cid.CID, k itemKind) ([]byte, error) {
	data, err := t.s.st.Get(c)
	if errors.Is(err, store.ErrNotFound) && k == kindBlob {
		b, err := t.s.st.OpenBlob(c.Multihash())
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		defer b.Close()
		if b.Size > t.inline {
			return nil, nil
		}
		return io.ReadAll(b.Reader())
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil || (k == kindBlob && uint64(len(data)) > t.inline) || (k == kindNode && len(data) > drisl.MaxNodeSize) {
		return nil, err
	}
	return data, nil
}

func (t *treeServe) visit(_ *loc, c cid.CID, k itemKind, shared func() bool) ([]byte, error) {
	if shared != nil && shared() {
		return nil, t.frame(frameHave, nil)
	}
	data, err := t.load(c, k)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, t.frame(frameMissing, nil)
	}
	if t.frames >= t.limit || (t.frames > 0 && t.bytes+uint64(len(data)) > uint64(t.s.cfg.TreeBytes)) {
		return nil, errBudget
	}
	t.frames++
	t.bytes += uint64(len(data))
	if err := t.frame(frameData, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (t *treeServe) end(more bool) error {
	b, err := drisl.Marshal(treeTrailer{More: more})
	if err != nil {
		return err
	}
	return t.frame(frameEnd, b)
}

func (s *Server) serveTree(b []byte, w *bufio.Writer) (*failure, error) {
	req, err := ParseTreeRequest(b)
	if err != nil {
		return fail(StatusBadRequest, err.Error()), nil
	}
	t := &treeServe{s: s, w: w, inline: req.inline(), limit: uint64(s.cfg.TreeFrames)}
	if len(req.Nodes) > 0 {
		t.inline = MaxInline
	}
	if req.Limit > 0 && req.Limit < t.limit {
		t.limit = req.Limit
	}
	if len(req.Nodes) == 0 {
		data, err := s.st.Get(req.Root)
		if errors.Is(err, store.ErrNotFound) {
			return fail(StatusNotFound, "root not held"), nil
		} else if err != nil {
			return fail(StatusInternal, "storage error"), err
		}
		if _, err := classifyRoot(data); err != nil {
			return fail(StatusBadRequest, "root is not a nu-tree node: "+err.Error()), nil
		}
	}
	walk := newTreeWalk(&req, s.st.Get)
	walk.visit = t.visit
	walk.hst = s.st
	walk.cache = map[cid.CID]*mst.Node[tree.Entry]{}
	err = walk.run()
	if !t.started {
		switch {
		case errors.Is(err, errCursor) && errors.Is(err, store.ErrNotFound):
			return fail(StatusNotFound, "cursor path not held"), nil
		case errors.Is(err, errCursor):
			return fail(StatusBadRequest, err.Error()), nil
		case err != nil && !errors.Is(err, errBudget):
			return fail(StatusInternal, "walk failed"), err
		}
	}
	switch {
	case err == nil:
		return nil, t.end(false)
	case errors.Is(err, errBudget):
		return nil, t.end(true)
	}
	return nil, err
}
