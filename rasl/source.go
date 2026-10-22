package rasl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/store"
)

// Store is where fetched objects go (store.Store): Put verifies a whole object, PutBlob
// verifies a raw blob of any size by its CID.
type Store interface {
	Put(c cid.CID, data []byte) error
	PutBlob(c cid.CID, r io.Reader, size int64) (cid.CID, error)
}

// Config is trusted local policy for a Source.
type Config struct {
	Client *httpsrc.Client
	Store  Store
	// Scheme is "https" (the default and the spec's). Tests set "http" together with the
	// client's AllowHTTP; hints are never plain HTTP otherwise.
	Scheme string
	// Dir stages BLAKE3 blobs before verification (the OS temp dir by default).
	Dir string
	// MaxBlobBytes bounds a BLAKE3 raw blob fetch (store.MaxBlobSize by default). Every
	// other object is a whole-object fetch under cid.MaxWholeObject.
	MaxBlobBytes int64
	// Timeout bounds one hint fetch (24 h); IdleTimeout bounds silence on it (1 min).
	Timeout, IdleTimeout time.Duration
	// OnMismatch, if set, is told of every URL that served bytes not matching the CID
	// (with the final URL after redirects), besides the origin demotion on Client. FISL
	// uses it to demote the record's publisher.
	OnMismatch func(url, finalURL string)
}

// Retriever fetches a CID from candidate URLs: the spec's "fetch a RASL URL" steps 2–8
// applied to each (a stateless GET, redirects as 307, the body as octet-stream, verified
// by CID before it is stored). RASL hints and FISL locations both resolve to it.
type Retriever struct {
	cfg Config
}

// NewRetriever applies the defaults to cfg.
func NewRetriever(cfg Config) *Retriever {
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	if cfg.MaxBlobBytes <= 0 {
		cfg.MaxBlobBytes = store.MaxBlobSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 24 * time.Hour
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Minute
	}
	return &Retriever{cfg: cfg}
}

// Source fetches objects from a fixed set of hinted hosts. It implements fetch.Source:
// the fetcher asks it for whole objects alongside Nu peers.
type Source struct {
	r     *Retriever
	hints []string
}

// NewSource returns a Source over hints (already validated by Parse).
func NewSource(cfg Config, hints []string) *Source {
	return &Source{r: NewRetriever(cfg), hints: append([]string(nil), hints...)}
}

// Hints returns the hosts in order.
func (s *Source) Hints() []string { return append([]string(nil), s.hints...) }

// FetchObject makes c held, verified, in the store, from the hints' RASL URLs (Fetch).
func (s *Source) FetchObject(ctx context.Context, c cid.CID, size uint64) error {
	urls := make([]string, len(s.hints))
	for i, h := range s.hints {
		urls[i] = HintURL(s.r.cfg.Scheme, h, c)
	}
	return s.r.Fetch(ctx, c, size, urls)
}

// ErrMismatch means a hint served bytes that do not hash to the requested CID; its
// final origin is demoted on the client.
var ErrMismatch = errors.New("rasl: served bytes do not match the CID")

// SourceError says why no location served the object, per URL.
type SourceError struct {
	CID      cid.CID
	Attempts []Attempt
}

// Attempt is one URL's failure.
type Attempt struct {
	URL string
	Err error
}

func (e *SourceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rasl: no location served %s", e.CID)
	for _, a := range e.Attempts {
		fmt.Fprintf(&b, "; %s: %v", a.URL, a.Err)
	}
	return b.String()
}

func (e *SourceError) Unwrap() []error {
	errs := make([]error, len(e.Attempts))
	for i, a := range e.Attempts {
		errs[i] = a.Err
	}
	return errs
}

// NotFound reports that every location answered that it has no such object (HTTP
// 404/410), as opposed to being unreachable, refusing, or serving wrong bytes.
func (e *SourceError) NotFound() bool {
	if len(e.Attempts) == 0 {
		return false
	}
	for _, a := range e.Attempts {
		var se *httpsrc.StatusError
		if !errors.As(a.Err, &se) || (se.Code != 404 && se.Code != 410) {
			return false
		}
	}
	return true
}

// errLost is an attempt that got a usable response after another hint had already won
// the race for the body; it is retried alone if the winner fails.
var errLost = errors.New("rasl: another hint answered first")

// Fetch makes c held, verified, in the store, from one of urls. size is the
// authenticated size when known (0 otherwise) and caps the download. All URLs on
// non-demoted origins are requested in parallel; the first usable response wins and the
// others are aborted (the spec's retrieval). If the winner fails mid-body or on
// verification, URLs that also answered are retried one at a time; demoted origins are
// tried only after every other URL. With no urls the error is a SourceError without
// attempts (NotFound is false).
func (s *Retriever) Fetch(ctx context.Context, c cid.CID, size uint64, urls []string) error {
	if len(urls) == 0 {
		return &SourceError{CID: c}
	}
	var fresh, demoted []string
	for _, u := range urls {
		if s.cfg.Client.Demoted(u) {
			demoted = append(demoted, u)
		} else {
			fresh = append(fresh, u)
		}
	}
	se := &SourceError{CID: c}
	for _, round := range [][]string{fresh, demoted} {
		if len(round) == 0 {
			continue
		}
		retry, err := s.race(ctx, round, c, size, se)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, u := range retry {
			if err := s.attempt(ctx, u, c, size, func() bool { return true }); err == nil {
				return nil
			} else if ctx.Err() != nil {
				return ctx.Err()
			} else {
				se.Attempts = append(se.Attempts, Attempt{u, err})
			}
		}
	}
	return se
}

// race runs one attempt per URL; the first to reach the body claims it and cancels the
// rest. It returns the URLs that had a usable response but lost, for a sequential retry.
func (s *Retriever) race(ctx context.Context, hints []string, c cid.CID, size uint64, se *SourceError) (retry []string, err error) {
	var mu sync.Mutex
	winner := -1
	cancels := make([]context.CancelFunc, len(hints))
	ctxs := make([]context.Context, len(hints))
	for i := range hints {
		ctxs[i], cancels[i] = context.WithCancel(ctx)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	errs := make([]error, len(hints))
	var wg sync.WaitGroup
	for i, h := range hints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.attempt(ctxs[i], h, c, size, func() bool {
				mu.Lock()
				defer mu.Unlock()
				if winner >= 0 {
					return false
				}
				winner = i
				for j, cancel := range cancels {
					if j != i {
						cancel()
					}
				}
				return true
			})
		}()
	}
	wg.Wait()
	for i, h := range hints {
		switch {
		case i == winner && errs[i] == nil:
			return nil, nil
		case i == winner:
			se.Attempts = append(se.Attempts, Attempt{h, errs[i]})
		case errors.Is(errs[i], errLost), errors.Is(errs[i], context.Canceled) && ctx.Err() == nil:
			retry = append(retry, h)
		default:
			se.Attempts = append(se.Attempts, Attempt{h, errs[i]})
		}
	}
	return retry, errors.New("rasl: race lost")
}

// attempt fetches c from one URL. claim is called once the response is usable and must
// report whether this attempt may consume the body.
func (s *Retriever) attempt(ctx context.Context, u string, c cid.CID, size uint64, claim func() bool) error {
	whole := cid.WholeObject(c.Codec(), c.Hash())
	opts := httpsrc.Options{MaxBytes: s.cfg.MaxBlobBytes, Timeout: s.cfg.Timeout, IdleTimeout: s.cfg.IdleTimeout}
	if whole {
		opts.MaxBytes = cid.MaxWholeObject
	}
	if size > 0 && int64(size) < opts.MaxBytes {
		opts.MaxBytes = int64(size)
	}
	var buf bytes.Buffer
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	sink := func(int64) (io.Writer, error) {
		if !claim() {
			return nil, errLost
		}
		if whole {
			buf.Reset()
			return &buf, nil
		}
		if f == nil {
			var cerr error
			if f, cerr = os.CreateTemp(s.cfg.Dir, "rasl-*"); cerr != nil {
				return nil, cerr
			}
			return f, nil
		}
		if terr := f.Truncate(0); terr != nil {
			return nil, terr
		}
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return nil, serr
		}
		return f, nil
	}
	res, err := s.cfg.Client.Fetch(ctx, u, sink, opts)
	if err != nil {
		return err
	}
	if whole {
		if err := c.Verify(buf.Bytes()); err != nil {
			s.mismatch(u, res.FinalURL)
			return fmt.Errorf("%w: %v", ErrMismatch, err)
		}
		return s.cfg.Store.Put(c, buf.Bytes())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := s.cfg.Store.PutBlob(c, f, res.Size); err != nil {
		if errors.Is(err, cid.ErrMismatch) {
			s.mismatch(u, res.FinalURL)
			return fmt.Errorf("%w: %v", ErrMismatch, err)
		}
		return err
	}
	return nil
}

func (s *Retriever) mismatch(u, final string) {
	s.cfg.Client.ReportVerificationFailure(final)
	if s.cfg.OnMismatch != nil {
		s.cfg.OnMismatch(u, final)
	}
}
