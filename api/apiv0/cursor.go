package apiv0

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Cursor is the decoded form of an opaque list cursor. Epoch identifies the generation
// of the listing (a daemon instance, a store generation); a cursor from another epoch has
// expired and the list answers Resync. Offset is the position after the last item seen.
type Cursor struct {
	Epoch  string
	Offset int
}

// Encode returns the opaque string form.
func (c Cursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.Epoch + ":" + strconv.Itoa(c.Offset)))
}

// ParseCursor decodes a cursor. An empty string is the start of the list. Malformed
// cursors are an invalid_argument error.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, Errorf(CodeInvalidArgument, "malformed cursor").WithAction("list again without a cursor")
	}
	epoch, off, ok := strings.Cut(string(b), ":")
	n, nerr := strconv.Atoi(off)
	if !ok || nerr != nil || n < 0 {
		return Cursor{}, Errorf(CodeInvalidArgument, "malformed cursor").WithAction("list again without a cursor")
	}
	return Cursor{Epoch: epoch, Offset: n}, nil
}

// DefaultPageLimit and MaxPageLimit bound list pages.
const (
	DefaultPageLimit = 100
	MaxPageLimit     = 1000
)

// PageSlice pages an in-memory, stably ordered slice that belongs to `epoch`. A cursor
// from a different epoch yields an empty page with Resync set. limit <= 0 uses the
// default; larger than MaxPageLimit is clamped.
func PageSlice[T any](items []T, cursor string, limit int, epoch string) (*Page[T], error) {
	c, err := ParseCursor(cursor)
	if err != nil {
		return nil, err
	}
	if cursor != "" && c.Epoch != epoch {
		return &Page[T]{Items: []T{}, Resync: true}, nil
	}
	if limit <= 0 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	start := min(c.Offset, len(items))
	end := min(start+limit, len(items))
	p := &Page[T]{Items: items[start:end]}
	if p.Items == nil {
		p.Items = []T{}
	}
	if end < len(items) {
		p.NextCursor = Cursor{Epoch: epoch, Offset: end}.Encode()
	}
	return p, nil
}
