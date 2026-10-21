package proto

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Protocol IDs and fixed limits of the data protocols (docs/data.md D14).
const (
	BytesID = "/nu/bytes/1"
	HaveID  = "/nu/have/1"

	// MaxRequestSize bounds an encoded request (the uvarint-framed DRISL map).
	MaxRequestSize = 16 << 10
	// MaxBytesHeaderSize bounds a /nu/bytes/1 response header.
	MaxBytesHeaderSize = 4 << 10
	// MaxHaveResponseSize bounds a /nu/have/1 response.
	MaxHaveResponseSize = 2 << 20
	// MaxRanges is the most group ranges one /nu/bytes/1 request may name.
	MaxRanges = 64
	// MaxHaveItems is the most items one /nu/have/1 request may name.
	MaxHaveItems = 256
	// MaxHaveWindow is the most chunk groups one /nu/have/1 item reports on.
	MaxHaveWindow = 32768
	// MaxHaveRangeList is the most held ranges sent as a list; more are sent as a bitmap.
	MaxHaveRangeList = 32
)

// Status is a response status code.
type Status uint64

const (
	StatusOK         Status = 0 // header is followed by the body
	StatusNotFound   Status = 1 // the peer holds no part of the object
	StatusNotHeld    Status = 2 // the peer holds the blob partially but not every requested group
	StatusBadRequest Status = 3 // malformed request, invalid ranges or limit exceeded
	StatusBusy       Status = 4 // too many concurrent requests from this peer or in total
	StatusInternal   Status = 5 // the peer failed locally (for example a storage error)
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusNotFound:
		return "not found"
	case StatusNotHeld:
		return "ranges not held"
	case StatusBadRequest:
		return "bad request"
	case StatusBusy:
		return "busy"
	case StatusInternal:
		return "internal error"
	}
	return fmt.Sprintf("status %d", uint64(s))
}

var errRequest = errors.New("proto: invalid request")

// BytesRequest asks for an object by multihash. Ranges (BLAKE3 only) are sorted,
// non-overlapping, non-empty chunk-group ranges; none means the whole object.
type BytesRequest struct {
	MH     cid.Multihash
	Ranges []blob.Range
}

type bytesRequestWire struct {
	MH     []byte      `drisl:"mh"`
	Ranges [][2]uint64 `drisl:"ranges,omitempty"`
}

// Marshal encodes the request as DRISL.
func (r BytesRequest) Marshal() ([]byte, error) {
	if err := checkRanges(r.MH, r.Ranges, MaxRanges); err != nil {
		return nil, err
	}
	return drisl.Marshal(bytesRequestWire{MH: r.MH.Bytes(), Ranges: rangesWire(r.Ranges)})
}

// ParseBytesRequest decodes and validates a request. Unknown keys are ignored.
func ParseBytesRequest(b []byte) (BytesRequest, error) {
	var w bytesRequestWire
	if err := (drisl.DecOptions{MaxSize: MaxRequestSize, MaxDepth: 8}).Unmarshal(b, &w); err != nil {
		return BytesRequest{}, fmt.Errorf("%w: %w", errRequest, err)
	}
	mh, err := cid.DecodeMultihash(w.MH)
	if err != nil {
		return BytesRequest{}, fmt.Errorf("%w: %w", errRequest, err)
	}
	r := BytesRequest{MH: mh, Ranges: rangesFromWire(w.Ranges)}
	return r, checkRanges(mh, r.Ranges, MaxRanges)
}

func checkRanges(mh cid.Multihash, rs []blob.Range, max int) error {
	if !mh.Hash.Valid() {
		return fmt.Errorf("%w: missing multihash", errRequest)
	}
	if len(rs) > 0 && mh.Hash != cid.BLAKE3 {
		return fmt.Errorf("%w: ranges need a BLAKE3 multihash", errRequest)
	}
	if len(rs) > max {
		return fmt.Errorf("%w: more than %d ranges", errRequest, max)
	}
	for i, r := range rs {
		if r.Start >= r.End || (i > 0 && rs[i-1].End > r.Start) {
			return fmt.Errorf("%w: ranges must be non-empty, sorted and disjoint", errRequest)
		}
	}
	return nil
}

func rangesWire(rs []blob.Range) [][2]uint64 {
	if len(rs) == 0 {
		return nil
	}
	out := make([][2]uint64, len(rs))
	for i, r := range rs {
		out[i] = [2]uint64{r.Start, r.End}
	}
	return out
}

func rangesFromWire(ws [][2]uint64) []blob.Range {
	if len(ws) == 0 {
		return nil
	}
	out := make([]blob.Range, len(ws))
	for i, w := range ws {
		out[i] = blob.Range{Start: w[0], End: w[1]}
	}
	return out
}

// bytesHeader is the /nu/bytes/1 response header.
type bytesHeader struct {
	Status uint64  `drisl:"status"`
	Error  string  `drisl:"error,omitempty"`
	Size   *uint64 `drisl:"size,omitempty"`
}

// HaveQuery asks whether a peer holds an object. Window (BLAKE3 blobs) limits the chunk
// groups reported for a partial blob; nil means from group 0.
type HaveQuery struct {
	MH     cid.Multihash
	Window *blob.Range
}

// HaveResult is one answer. Ranges are the held groups inside Window, which is the whole
// blob for complete objects and at most MaxHaveWindow groups for partial ones.
type HaveResult struct {
	Held     bool
	Codec    cid.Codec
	Size     uint64
	Complete bool
	Window   blob.Range
	Ranges   []blob.Range
}

type haveRequestWire struct {
	Want []haveItemWire `drisl:"want"`
}

type haveItemWire struct {
	MH    []byte     `drisl:"mh"`
	Range *[2]uint64 `drisl:"range,omitempty"`
}

type haveResponseWire struct {
	Status uint64         `drisl:"status"`
	Error  string         `drisl:"error,omitempty"`
	Have   []byte         `drisl:"have,omitempty"`
	Info   []haveInfoWire `drisl:"info,omitempty"`
}

type haveInfoWire struct {
	Codec    uint64      `drisl:"codec"`
	Size     uint64      `drisl:"size"`
	Complete bool        `drisl:"complete,omitempty"`
	Window   *[2]uint64  `drisl:"window,omitempty"`
	Ranges   [][2]uint64 `drisl:"ranges,omitempty"`
	Bits     []byte      `drisl:"bits,omitempty"`
}

// MarshalHaveRequest encodes a /nu/have/1 request.
func MarshalHaveRequest(qs []HaveQuery) ([]byte, error) {
	if len(qs) == 0 || len(qs) > MaxHaveItems {
		return nil, fmt.Errorf("%w: 1 to %d items", errRequest, MaxHaveItems)
	}
	w := haveRequestWire{Want: make([]haveItemWire, len(qs))}
	for i, q := range qs {
		if !q.MH.Hash.Valid() || (q.Window != nil && (q.MH.Hash != cid.BLAKE3 || q.Window.Start >= q.Window.End)) {
			return nil, fmt.Errorf("%w: item %d", errRequest, i)
		}
		w.Want[i].MH = q.MH.Bytes()
		if q.Window != nil {
			w.Want[i].Range = &[2]uint64{q.Window.Start, q.Window.End}
		}
	}
	return drisl.Marshal(w)
}

// ParseHaveRequest decodes and validates a /nu/have/1 request. Unknown keys are ignored.
func ParseHaveRequest(b []byte) ([]HaveQuery, error) {
	var w haveRequestWire
	if err := (drisl.DecOptions{MaxSize: MaxRequestSize, MaxDepth: 8}).Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("%w: %w", errRequest, err)
	}
	if len(w.Want) == 0 || len(w.Want) > MaxHaveItems {
		return nil, fmt.Errorf("%w: 1 to %d items", errRequest, MaxHaveItems)
	}
	qs := make([]HaveQuery, len(w.Want))
	for i, it := range w.Want {
		mh, err := cid.DecodeMultihash(it.MH)
		if err != nil {
			return nil, fmt.Errorf("%w: item %d: %w", errRequest, i, err)
		}
		qs[i].MH = mh
		if it.Range != nil {
			if mh.Hash != cid.BLAKE3 || it.Range[0] >= it.Range[1] {
				return nil, fmt.Errorf("%w: item %d: bad range", errRequest, i)
			}
			qs[i].Window = &blob.Range{Start: it.Range[0], End: it.Range[1]}
		}
	}
	return qs, nil
}

// haveAnswer builds the response entry for a held object.
func haveAnswer(res HaveResult) haveInfoWire {
	in := haveInfoWire{Codec: uint64(res.Codec), Size: res.Size, Complete: res.Complete}
	if res.Complete {
		return in
	}
	in.Window = &[2]uint64{res.Window.Start, res.Window.End}
	if len(res.Ranges) <= MaxHaveRangeList {
		in.Ranges = rangesWire(res.Ranges)
		return in
	}
	n := res.Window.End - res.Window.Start
	in.Bits = make([]byte, (n+7)/8)
	for _, r := range res.Ranges {
		for g := r.Start; g < r.End; g++ {
			i := g - res.Window.Start
			in.Bits[i/8] |= 1 << (i % 8)
		}
	}
	return in
}

// parseHaveResponse decodes and validates a /nu/have/1 response for n queries.
func parseHaveResponse(b []byte, n int) (Status, string, []HaveResult, error) {
	var w haveResponseWire
	if err := (drisl.DecOptions{MaxSize: MaxHaveResponseSize, MaxDepth: 8}).Unmarshal(b, &w); err != nil {
		return 0, "", nil, err
	}
	if Status(w.Status) != StatusOK {
		return Status(w.Status), w.Error, nil, nil
	}
	if len(w.Have) != (n+7)/8 {
		return 0, "", nil, errors.New("presence bitmap length")
	}
	held := 0
	for _, x := range w.Have {
		held += bits.OnesCount8(x)
	}
	if n%8 != 0 && w.Have[len(w.Have)-1]>>(n%8) != 0 {
		return 0, "", nil, errors.New("presence bitmap padding")
	}
	if held != len(w.Info) {
		return 0, "", nil, errors.New("info count does not match bitmap")
	}
	out := make([]HaveResult, n)
	k := 0
	for i := range out {
		if w.Have[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		in := w.Info[k]
		k++
		r := HaveResult{Held: true, Codec: cid.Codec(in.Codec), Size: in.Size, Complete: in.Complete}
		if !r.Codec.Valid() {
			return 0, "", nil, errors.New("invalid codec")
		}
		if r.Complete {
			r.Window = blob.Range{Start: 0, End: blob.Groups(r.Size)}
			r.Ranges = []blob.Range{r.Window}
			out[i] = r
			continue
		}
		if in.Window == nil || in.Window[0] > in.Window[1] || in.Window[1] > blob.Groups(r.Size) || in.Window[1]-in.Window[0] > MaxHaveWindow {
			return 0, "", nil, errors.New("invalid window")
		}
		r.Window = blob.Range{Start: in.Window[0], End: in.Window[1]}
		if in.Bits != nil {
			if in.Ranges != nil || uint64(len(in.Bits)) != (r.Window.End-r.Window.Start+7)/8 {
				return 0, "", nil, errors.New("invalid bitmap")
			}
			for g := r.Window.Start; g < r.Window.End; g++ {
				j := g - r.Window.Start
				if in.Bits[j/8]&(1<<(j%8)) == 0 {
					continue
				}
				if l := len(r.Ranges); l > 0 && r.Ranges[l-1].End == g {
					r.Ranges[l-1].End++
				} else {
					r.Ranges = append(r.Ranges, blob.Range{Start: g, End: g + 1})
				}
			}
		} else {
			r.Ranges = rangesFromWire(in.Ranges)
			if len(r.Ranges) > MaxHaveRangeList {
				return 0, "", nil, errors.New("too many ranges")
			}
			for j, g := range r.Ranges {
				if g.Start >= g.End || g.Start < r.Window.Start || g.End > r.Window.End || (j > 0 && r.Ranges[j-1].End > g.Start) {
					return 0, "", nil, errors.New("invalid held ranges")
				}
			}
		}
		out[i] = r
	}
	return StatusOK, "", out, nil
}
