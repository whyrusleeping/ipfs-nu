package proto

import (
	"context"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// Challenge asks one peer for chunk group g of a blob and verifies the answer against
// the CID (cluster audits, docs/cluster.md "Scrub and audit"): a BLAKE3 blob by its Bao
// slice, a SHA-256 object whole. Nothing is stored, and the challenger need not hold the
// blob. It returns the size the peer reported. A passed challenge shows the peer served
// verified bytes in time, not that it stores them (E9).
func Challenge(ctx context.Context, open Opener, mh cid.Multihash, g uint64) (uint64, error) {
	if mh.Hash == cid.SHA256 {
		data, err := fetchWhole(ctx, open, mh, cid.MaxWholeObject)
		return uint64(len(data)), err
	}
	rg := blob.Range{Start: g, End: g + 1}
	c, size, err := bytesCall(ctx, open, BytesRequest{MH: mh, Ranges: []blob.Range{rg}})
	if err != nil {
		return 0, err
	}
	if !rg.Valid(size) {
		return size, c.end(corrupt("group %d outside a %d-byte blob", g, size))
	}
	if err := readSlice(c.r, mh, size, rg, blob.Sink{Data: discardAt{}}); err != nil {
		return size, c.end(err)
	}
	return size, c.end(c.eof())
}

type discardAt struct{}

func (discardAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }
