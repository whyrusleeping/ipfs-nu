package httpsrc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"lukechampine.com/blake3"
)

// URL import (docs/data.md D17): fetch a URL, hash the bytes into the store as a blob,
// optionally verify a caller-supplied checksum, and resume interrupted downloads with
// Range/If-Range on a strong validator.

// ErrChecksum means the downloaded bytes do not match the supplied checksum.
var ErrChecksum = errors.New("httpsrc: checksum mismatch")

// BlobAdder stores a blob of known size (store.Store).
type BlobAdder interface {
	AddBlobHash(r io.Reader, size int64, h cid.Hash) (cid.CID, error)
}

// Checksum is a caller-supplied multihash, e.g. a STAC `file:checksum`. Supported
// functions are sha2-256 (0x12), sha2-512 (0x13) and blake3 (0x1e, 32 bytes).
type Checksum struct {
	Code   uint64
	Digest []byte
}

// ParseChecksum parses a hex-encoded multihash.
func ParseChecksum(s string) (Checksum, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return Checksum{}, fmt.Errorf("checksum %q is not a hex multihash", s)
	}
	code, n := binary.Uvarint(b)
	if n <= 0 {
		return Checksum{}, fmt.Errorf("checksum %q is not a multihash", s)
	}
	size, m := binary.Uvarint(b[n:])
	if m <= 0 || uint64(len(b)-n-m) != size {
		return Checksum{}, fmt.Errorf("checksum %q has a bad digest length", s)
	}
	c := Checksum{Code: code, Digest: b[n+m:]}
	want := map[uint64]int{0x12: 32, 0x13: 64, 0x1e: 32}[code]
	if want == 0 {
		return Checksum{}, fmt.Errorf("checksum %q: multihash function 0x%x is not supported (sha2-256, sha2-512 or blake3)", s, code)
	}
	if len(c.Digest) != want {
		return Checksum{}, fmt.Errorf("checksum %q: digest must be %d bytes", s, want)
	}
	return c, nil
}

func (c Checksum) String() string {
	b := binary.AppendUvarint(nil, c.Code)
	b = binary.AppendUvarint(b, uint64(len(c.Digest)))
	return hex.EncodeToString(append(b, c.Digest...))
}

func (c Checksum) newHash() hash.Hash {
	switch c.Code {
	case 0x12:
		return sha256.New()
	case 0x13:
		return sha512.New()
	}
	return blake3.New(32, nil)
}

// ImportOptions configures Import. Options carries the per-attempt limits and the host
// allowlist; MaxBytes caps the file (SHA-256 imports are capped at the 8 MiB
// whole-object limit regardless).
type ImportOptions struct {
	Options
	// Hash is the blob hash (BLAKE3 by default).
	Hash cid.Hash
	// Checksum, when set, is verified before the import succeeds; a mismatch is
	// ErrChecksum and demotes the final origin.
	Checksum *Checksum
	// Size, when positive, is the expected size; anything else fails.
	Size int64
	// Partial is the file that keeps downloaded bytes between calls (with the response
	// validators in Partial+".json"), so a later Import resumes where this one stopped.
	// It is removed once the import succeeds or fails permanently. Empty: a temporary
	// file in Dir, removed when Import returns.
	Partial string
	Dir     string
	// Attempts bounds the fetches of this call (default 1); Backoff is the delay before
	// the second attempt, doubling up to a minute (default 1 s).
	Attempts int
	Backoff  time.Duration
	// Progress, when set, is called with the bytes held so far as they arrive.
	Progress func(int64)
}

type progressWriter struct {
	w  io.Writer
	n  int64
	cb func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	p.cb(p.n)
	return n, err
}

// Imported is a stored URL import with its provenance.
type Imported struct {
	CID cid.CID
	Result
	// Resumed reports that a retained prefix was reused rather than fetched again.
	Resumed bool
}

// Check reports whether rawURL passes the URL policy for hosts (no network access).
func (c *Client) Check(rawURL string, hosts []string) error {
	if len(rawURL) > MaxURLBytes {
		return fmt.Errorf("%w: URL too long", ErrPolicy)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPolicy, err)
	}
	return c.checkURL(u, hosts)
}

// Permanent reports whether an import error will recur on retry: policy denials, size
// limits, checksum mismatches and HTTP 4xx other than 408 and 429.
func Permanent(err error) bool {
	var se *StatusError
	switch {
	case errors.Is(err, ErrPolicy), errors.Is(err, ErrTooLarge), errors.Is(err, ErrChecksum), errors.Is(err, cid.ErrTooLarge):
		return true
	case errors.As(err, &se):
		return se.Code >= 400 && se.Code < 500 && se.Code != 408 && se.Code != 429 && se.Code != 416
	}
	return false
}

// Import fetches rawURL into st. See ImportOptions for resume and verification.
func (c *Client) Import(ctx context.Context, rawURL string, st BlobAdder, o ImportOptions) (Imported, error) {
	if o.Hash == 0 {
		o.Hash = cid.BLAKE3
	}
	if o.Hash == cid.SHA256 && (o.MaxBytes <= 0 || o.MaxBytes > cid.MaxWholeObject) {
		o.MaxBytes = cid.MaxWholeObject
	}
	if o.Size > 0 {
		if o.Size > o.MaxBytes {
			return Imported{}, fmt.Errorf("%w: declared size %d exceeds the limit %d", ErrTooLarge, o.Size, o.MaxBytes)
		}
		o.MaxBytes = o.Size
	}
	if o.Attempts <= 0 {
		o.Attempts = 1
	}
	if o.Backoff <= 0 {
		o.Backoff = time.Second
	}
	var f *os.File
	var err error
	if o.Partial == "" {
		if f, err = os.CreateTemp(o.Dir, "import-*"); err != nil {
			return Imported{}, err
		}
		defer os.Remove(f.Name())
	} else if f, err = os.OpenFile(o.Partial, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return Imported{}, err
	}
	defer f.Close()
	discard := func() {
		if o.Partial != "" {
			_ = os.Remove(o.Partial)
			_ = os.Remove(o.Partial + ".json")
		}
	}
	var resume *Result
	if o.Partial != "" {
		if b, err := os.ReadFile(o.Partial + ".json"); err == nil {
			var r Result
			fi, ferr := f.Stat()
			if json.Unmarshal(b, &r) == nil && ferr == nil && fi.Size() > 0 && r.FinalURL != "" && r.validator() != "" {
				r.Size = fi.Size()
				resume = &r
			}
		}
	}
	opened, resumed := false, false
	sink := func(offset int64) (io.Writer, error) {
		opened, resumed = true, offset > 0
		if err := f.Truncate(offset); err != nil {
			return nil, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		if o.Progress != nil {
			return &progressWriter{w: f, n: offset, cb: o.Progress}, nil
		}
		return f, nil
	}
	drop := func() {
		resume = nil
		_ = f.Truncate(0)
		if o.Partial != "" {
			_ = os.Remove(o.Partial + ".json")
		}
	}
	var res Result
	backoff := o.Backoff
	for attempt := 1; ; {
		target := rawURL
		opts := o.Options
		if resume != nil {
			target, opts.Resume = resume.FinalURL, resume
		}
		opened = false
		res, err = c.Fetch(ctx, target, sink, opts)
		if err == nil {
			break
		}
		switch {
		case opened && res.Size > 0 && res.validator() != "":
			// A body error after streaming began: keep the bytes for the next attempt.
			r := res
			resume = &r
			if o.Partial != "" {
				if b, jerr := json.Marshal(res); jerr == nil {
					_ = os.WriteFile(o.Partial+".json", b, 0o600)
				}
			}
		case opened:
			drop() // nothing that can resume
		case ctx.Err() != nil:
		case resume != nil && (errors.Is(err, ErrResponse) || Permanent(err)):
			// The retained prefix cannot be resumed (changed resource, expired final
			// URL, bad range): start over from the original URL; this is not an attempt.
			drop()
			continue
		}
		if ctx.Err() != nil { // stopped (a shutdown): what can resume stays for the next call
			return Imported{Result: res}, ctx.Err()
		}
		if Permanent(err) {
			discard()
			return Imported{Result: res}, err
		}
		if attempt >= o.Attempts {
			return Imported{Result: res}, err
		}
		attempt++
		select {
		case <-ctx.Done():
			return Imported{Result: res}, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
	if o.Size > 0 && res.Size != o.Size {
		discard()
		return Imported{Result: res}, fmt.Errorf("%w: got %d bytes, declared %d", ErrResponse, res.Size, o.Size)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Imported{Result: res}, err
	}
	var r io.Reader = f
	var hs hash.Hash
	if o.Checksum != nil {
		hs = o.Checksum.newHash()
		r = io.TeeReader(f, hs)
	}
	got, err := st.AddBlobHash(r, res.Size, o.Hash)
	if err != nil {
		return Imported{Result: res}, err // the download stays for the next attempt
	}
	discard()
	if hs != nil && !bytes.Equal(hs.Sum(nil), o.Checksum.Digest) {
		c.ReportVerificationFailure(res.FinalURL)
		return Imported{Result: res}, fmt.Errorf("%w: %s does not match %s", ErrChecksum, res.FinalURL, o.Checksum)
	}
	return Imported{CID: got, Result: res, Resumed: resumed}, nil
}
