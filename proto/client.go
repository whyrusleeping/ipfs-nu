package proto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// ErrCorrupt reports that a peer sent data that failed verification or broke the protocol
// (bad header, size mismatch, oversized whole object, trailing bytes). Nothing unverified was
// stored. Callers should demote or drop the peer. Errors wrap it, and a *blob.CorruptError
// when a chunk group failed.
var ErrCorrupt = errors.New("proto: peer sent corrupt data")

// StatusError is a refusal from the peer. It matches the Err* status sentinels with
// errors.Is. Any other error from a fetch is a transport or local error (connection lost,
// stream reset, cancellation, storage) and says nothing about the peer's honesty.
type StatusError struct {
	Status  Status
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return "proto: peer refused: " + e.Status.String()
	}
	return "proto: peer refused: " + e.Status.String() + ": " + e.Message
}

func (e *StatusError) Is(target error) bool {
	t, ok := target.(*StatusError)
	return ok && t.Message == "" && t.Status == e.Status
}

// Sentinels for errors.Is against a *StatusError.
var (
	ErrNotFound = &StatusError{Status: StatusNotFound}
	ErrNotHeld  = &StatusError{Status: StatusNotHeld}
	ErrBusy     = &StatusError{Status: StatusBusy}
)

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, a...)...)
}

// Stream is a client stream for one request.
type Stream interface {
	io.ReadWriter
	CloseWrite() error
	Close() error
	Reset()
}

// Opener opens a new stream to one peer for a protocol. Each request uses its own stream.
type Opener func(ctx context.Context, protocol string) (Stream, error)

// HostOpener opens optimistic-select streams to a connected peer.
func HostOpener(h *host.Host, peer wire.PeerID) Opener {
	return func(ctx context.Context, protocol string) (Stream, error) {
		s, err := h.OpenStreamLazy(ctx, peer, protocol)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// call is one request in flight. Cancelling ctx resets the stream.
type call struct {
	ctx  context.Context
	s    Stream
	r    *bufio.Reader
	stop func() bool
}

func start(ctx context.Context, open Opener, protocol string, req []byte) (*call, error) {
	s, err := open(ctx, protocol)
	if err != nil {
		return nil, err
	}
	c := &call{ctx: ctx, s: s, r: bufio.NewReaderSize(s, 64<<10), stop: context.AfterFunc(ctx, s.Reset)}
	if err := wire.WriteMessage(s, req, MaxRequestSize); err != nil {
		return nil, c.end(err)
	}
	_ = s.CloseWrite() // the server may already have stopped reading after a refusal
	return c, nil
}

// header reads a response header into v, returning a *StatusError for refusals.
func (c *call) header(max int, v any, status func() (Status, string)) error {
	b, err := wire.ReadMessage(c.r, max)
	if errors.Is(err, wire.ErrLimit) {
		return corrupt("response header over %d bytes", max)
	}
	if err != nil {
		return err
	}
	if err := (drisl.DecOptions{MaxSize: max, MaxDepth: 8}).Unmarshal(b, v); err != nil {
		return corrupt("response header: %w", err)
	}
	if st, msg := status(); st != StatusOK {
		return &StatusError{Status: st, Message: msg}
	}
	return nil
}

// eof checks that the body ended exactly.
func (c *call) eof() error {
	if _, err := c.r.ReadByte(); err == nil {
		return corrupt("trailing bytes after the response body")
	} else if err != io.EOF {
		return err
	}
	return nil
}

// end finishes the call: on error the stream is reset; cancellation wins over the error it
// caused.
func (c *call) end(err error) error {
	if !c.stop() {
		c.s.Reset()
		return errors.Join(c.ctx.Err(), err)
	}
	if err != nil {
		c.s.Reset()
		return err
	}
	return c.s.Close()
}

// bytesCall sends a /nu/bytes/1 request and reads the header, returning the object size.
func bytesCall(ctx context.Context, open Opener, req BytesRequest) (*call, uint64, error) {
	b, err := req.Marshal()
	if err != nil {
		return nil, 0, err
	}
	c, err := start(ctx, open, BytesID, b)
	if err != nil {
		return nil, 0, err
	}
	var h bytesHeader
	if err := c.header(MaxBytesHeaderSize, &h, func() (Status, string) { return Status(h.Status), h.Error }); err != nil {
		return nil, 0, c.end(err)
	}
	if h.Size == nil {
		return nil, 0, c.end(corrupt("response header without size"))
	}
	return c, *h.Size, nil
}

// readSlice decodes one Bao slice, classifying verification failures as corrupt.
func readSlice(r io.Reader, mh cid.Multihash, size uint64, rg blob.Range, sink blob.Sink) error {
	err := blob.DecodeSlice(r, mh, size, rg, sink)
	if errors.Is(err, blob.ErrCorrupt) {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return err
}

// FetchRanges fetches the chunk-group ranges of partial blob p from one peer, at most
// MaxRanges per request. Each group is verified before it is written to p, so after any
// error p holds exactly the groups that verified; fetch p.Missing() again to resume. Several
// calls may fill disjoint ranges of the same p concurrently. The caller flushes or commits p.
func FetchRanges(ctx context.Context, open Opener, p *store.Partial, ranges []blob.Range) error {
	if len(ranges) == 0 || len(ranges) > MaxRanges {
		return fmt.Errorf("proto: 1 to %d ranges per request", MaxRanges)
	}
	for _, r := range ranges {
		if !r.Valid(p.Size) {
			return fmt.Errorf("proto: range %v outside a %d-byte blob", r, p.Size)
		}
	}
	c, size, err := bytesCall(ctx, open, BytesRequest{MH: p.MH, Ranges: ranges})
	if err != nil {
		return err
	}
	if size != p.Size {
		return c.end(corrupt("peer reports size %d, want %d", size, p.Size))
	}
	for _, r := range ranges {
		if err := readSlice(c.r, p.MH, p.Size, r, p.Sink()); err != nil {
			return c.end(err)
		}
	}
	return c.end(c.eof())
}

// FetchWhole fetches a whole object from one peer into memory, verifies it and only then
// stores it. It is the path for every whole-object-verified CID (SHA-256 of any codec and
// BLAKE3 DRISL) and for BLAKE3 blobs of at most one chunk group; peers announcing more than
// cid.MaxWholeObject bytes are refused before any body is read.
func FetchWhole(ctx context.Context, open Opener, st *store.Store, c cid.CID) error {
	data, err := fetchWhole(ctx, open, c.Multihash(), cid.MaxWholeObject)
	if err != nil {
		return err
	}
	return st.Put(c, data)
}

func fetchWhole(ctx context.Context, open Opener, mh cid.Multihash, max uint64) ([]byte, error) {
	cl, size, err := bytesCall(ctx, open, BytesRequest{MH: mh})
	if err != nil {
		return nil, err
	}
	if size > max {
		return nil, cl.end(corrupt("peer announces a %d-byte whole object; the limit is %d", size, max))
	}
	data := make(memAt, size)
	if mh.Hash == cid.SHA256 {
		if _, err := io.ReadFull(cl.r, data); err != nil {
			return nil, cl.end(err)
		}
		if cid.Sum(mh.Hash, data) != mh {
			return nil, cl.end(corrupt("object does not match %s", mh))
		}
	} else if err := readSlice(cl.r, mh, size, blob.Range{Start: 0, End: blob.Groups(size)}, blob.Sink{Data: data}); err != nil {
		return nil, cl.end(err)
	}
	return data, cl.end(cl.eof())
}

// Fetch fetches a whole object from one peer into st. size is the object size from an
// authenticated source (tree entry, file node), or -1 if unknown, in which case the size the
// peer reports is used and is only proven when the blob completes. Large BLAKE3 blobs go
// through a store.Partial, resuming from groups already held, and are committed when
// complete; everything else uses FetchWhole.
func Fetch(ctx context.Context, open Opener, st *store.Store, c cid.CID, size int64) error {
	if info, ok, err := st.Has(c); err != nil {
		return err
	} else if ok && info.Complete {
		return nil
	} else if ok && size < 0 {
		size = int64(info.Size)
	}
	if cid.WholeObject(c.Codec(), c.Hash()) || (size >= 0 && !blob.HasOutboard(uint64(size))) {
		return FetchWhole(ctx, open, st, c)
	}
	if size < 0 {
		return fetchUnknown(ctx, open, st, c)
	}
	p, err := st.Partial(c.Multihash(), uint64(size))
	if errors.Is(err, store.ErrComplete) {
		return nil
	} else if err != nil {
		return err
	}
	for {
		missing := p.Missing()
		if len(missing) == 0 {
			if err := p.Commit(); err != nil {
				return errors.Join(err, p.Close())
			}
			return nil
		}
		if err := FetchRanges(ctx, open, p, missing[:min(len(missing), MaxRanges)]); err != nil {
			return errors.Join(err, p.Close())
		}
	}
}

// fetchUnknown fetches a BLAKE3 raw blob of unknown size in one request, choosing the whole
// or partial path from the size in the response header.
func fetchUnknown(ctx context.Context, open Opener, st *store.Store, c cid.CID) error {
	mh := c.Multihash()
	cl, size, err := bytesCall(ctx, open, BytesRequest{MH: mh})
	if err != nil {
		return err
	}
	whole := blob.Range{Start: 0, End: blob.Groups(size)}
	if !blob.HasOutboard(size) {
		data := make(memAt, size)
		if err := readSlice(cl.r, mh, size, whole, blob.Sink{Data: data}); err != nil {
			return cl.end(err)
		}
		if err := cl.end(cl.eof()); err != nil {
			return err
		}
		return st.Put(c, data)
	}
	p, err := st.Partial(mh, size)
	if err != nil {
		return cl.end(err)
	}
	if err := readSlice(cl.r, mh, size, whole, p.Sink()); err != nil {
		return errors.Join(cl.end(err), p.Close())
	}
	if err := cl.end(cl.eof()); err != nil {
		return errors.Join(err, p.Close())
	}
	return p.Commit()
}

// Have asks one peer which of the queried objects it holds. Results align with qs.
func Have(ctx context.Context, open Opener, qs []HaveQuery) ([]HaveResult, error) {
	b, err := MarshalHaveRequest(qs)
	if err != nil {
		return nil, err
	}
	c, err := start(ctx, open, HaveID, b)
	if err != nil {
		return nil, err
	}
	msg, err := wire.ReadMessage(c.r, MaxHaveResponseSize)
	if errors.Is(err, wire.ErrLimit) {
		err = corrupt("have response over %d bytes", MaxHaveResponseSize)
	}
	if err != nil {
		return nil, c.end(err)
	}
	status, text, res, err := parseHaveResponse(msg, len(qs))
	if err != nil {
		return nil, c.end(corrupt("have response: %w", err))
	}
	if status != StatusOK {
		return nil, c.end(&StatusError{Status: status, Message: text})
	}
	for i, r := range res {
		if r.Held && qs[i].MH.Hash != cid.BLAKE3 && !r.Complete {
			return nil, c.end(corrupt("partial answer for a whole object"))
		}
	}
	return res, c.end(c.eof())
}
