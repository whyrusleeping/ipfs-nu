package fisl

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/rasl"
)

// Config is trusted local policy for a Source.
type Config struct {
	// Indexers are base URLs (ParseIndexer form). Each is asked for every object.
	Indexers []string
	// Retrieve is how located https URLs are fetched (Client, Store, Dir, limits); its
	// OnMismatch is chained to the publisher demotion.
	Retrieve rasl.Config
	// AllowHTTP uses http:// locations too (tests and private networks; the spec says
	// to skip them otherwise). The httpsrc client must allow them as well.
	AllowHTTP bool
	// LookupTimeout bounds one indexer lookup (15 s); IdleTimeout bounds silence on it
	// (10 s).
	LookupTimeout, IdleTimeout time.Duration
	// MaxCandidates bounds the URLs tried per object (rasl.MaxHints).
	MaxCandidates int
	// Demotion is how long a publisher whose location served wrong bytes is tried last
	// (5 min, matching httpsrc's origin demotion).
	Demotion time.Duration
	// Now is the clock for expiry (time.Now).
	Now func() time.Time
}

// Source finds objects through FISL indexers and fetches them from the located https
// URLs. It implements fetch.Source: the fetcher asks it for whole objects alongside Nu
// peers.
type Source struct {
	cfg    Config
	r      *rasl.Retriever
	client *httpsrc.Client

	mu        sync.Mutex
	demoted   map[string]time.Time // publisher DID → until
	publisher map[string]string    // location URL → publisher, for the mismatch hook
}

const maxDemoted = 256

// NewSource returns a Source over cfg.Indexers (already validated by ParseIndexer).
func NewSource(cfg Config) *Source {
	if cfg.LookupTimeout <= 0 {
		cfg.LookupTimeout = 15 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Second
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = rasl.MaxHints
	}
	if cfg.Demotion <= 0 {
		cfg.Demotion = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Indexers = append([]string(nil), cfg.Indexers...)
	s := &Source{cfg: cfg, client: cfg.Retrieve.Client, demoted: map[string]time.Time{}, publisher: map[string]string{}}
	inner := cfg.Retrieve.OnMismatch
	cfg.Retrieve.OnMismatch = func(u, final string) {
		s.mismatch(u)
		if inner != nil {
			inner(u, final)
		}
	}
	s.r = rasl.NewRetriever(cfg.Retrieve)
	return s
}

// Indexers returns the configured base URLs.
func (s *Source) Indexers() []string { return append([]string(nil), s.cfg.Indexers...) }

// ErrNoLocation means every indexer answered and none listed a usable, unexpired
// location for the CID.
var ErrNoLocation = errors.New("fisl: no indexer knows a location")

// LookupError says why no indexer answered, per indexer.
type LookupError struct {
	CID      cid.CID
	Attempts []rasl.Attempt
}

func (e *LookupError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fisl: no indexer answered for %s", e.CID)
	for _, a := range e.Attempts {
		fmt.Fprintf(&b, "; %s: %v", a.URL, a.Err)
	}
	return b.String()
}

func (e *LookupError) Unwrap() []error {
	errs := make([]error, len(e.Attempts))
	for i, a := range e.Attempts {
		errs[i] = a.Err
	}
	return errs
}

// Lookup asks every indexer for c, in parallel, and returns the unexpired records they
// returned (indexer order). It fails only when no indexer answered usably.
func (s *Source) Lookup(ctx context.Context, c cid.CID) ([]Location, error) {
	results := make([][]Location, len(s.cfg.Indexers))
	errs := make([]error, len(s.cfg.Indexers))
	var wg sync.WaitGroup
	for i, idx := range s.cfg.Indexers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.lookup(ctx, idx, c)
		}()
	}
	wg.Wait()
	le := &LookupError{CID: c}
	var out []Location
	answered := false
	for i, idx := range s.cfg.Indexers {
		if errs[i] != nil {
			le.Attempts = append(le.Attempts, rasl.Attempt{URL: idx, Err: errs[i]})
		} else {
			answered = true
		}
		out = append(out, results[i]...)
	}
	if !answered {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, le
	}
	return out, nil
}

// lookup asks one indexer, in jsonl, falling back to json when the indexer says it does
// not serve jsonl (404). Records parsed before a malformed line are kept.
func (s *Source) lookup(ctx context.Context, indexer string, c cid.CID) ([]Location, error) {
	locs, err := s.lookupFormat(ctx, indexer, c, "jsonl")
	var se *httpsrc.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		locs, err = s.lookupFormat(ctx, indexer, c, "json")
	}
	return locs, err
}

func (s *Source) lookupFormat(ctx context.Context, indexer string, c cid.CID, format string) ([]Location, error) {
	req, err := http.NewRequest(http.MethodGet, LookupURL(indexer, c.String(), format), nil)
	if err != nil {
		return nil, err
	}
	opts := httpsrc.Options{MaxBytes: MaxResponseBytes, Timeout: s.cfg.LookupTimeout, IdleTimeout: s.cfg.IdleTimeout}
	resp, err := s.client.Do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpsrc.StatusError{Code: resp.StatusCode}
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	now := s.cfg.Now()
	switch format {
	case "jsonl":
		if mt != "application/jsonl" && mt != "application/x-ndjson" && mt != "application/json" {
			return nil, fmt.Errorf("%w: content type %q", ErrFormat, resp.Header.Get("Content-Type"))
		}
		locs, err := DecodeJSONL(resp.Body, now)
		if err != nil && len(locs) == 0 {
			return nil, err
		}
		return locs, nil // records before a malformed line or the size limit are kept
	default:
		if mt != "application/json" {
			return nil, fmt.Errorf("%w: content type %q", ErrFormat, resp.Header.Get("Content-Type"))
		}
		return DecodeJSON(resp.Body, now)
	}
}

// Candidates selects the URLs to try for c from locs: supported schemes only, without
// duplicates, at most MaxCandidates, split into fresh and demoted (publisher demoted
// here, or origin demoted on the httpsrc client). It records each URL's publisher for
// the mismatch hook.
func (s *Source) Candidates(locs []Location) (fresh, demoted []string) {
	now := s.cfg.Now()
	seen := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, loc := range locs {
		for _, a := range loc.Addrs {
			if len(seen) >= s.cfg.MaxCandidates {
				return
			}
			u, err := url.Parse(a.URL)
			if err != nil || seen[a.URL] || (u.Scheme != "https" && !(s.cfg.AllowHTTP && u.Scheme == "http")) {
				continue
			}
			seen[a.URL] = true
			if loc.Publisher != "" {
				if len(s.publisher) >= 4096 {
					clear(s.publisher)
				}
				s.publisher[a.URL] = loc.Publisher
			}
			if (loc.Publisher != "" && s.demoted[loc.Publisher].After(now)) || s.client.Demoted(a.URL) {
				demoted = append(demoted, a.URL)
			} else {
				fresh = append(fresh, a.URL)
			}
		}
	}
	return
}

// mismatch demotes the publisher of a location that served wrong bytes (the origin is
// demoted by the retriever on the httpsrc client; a record without a publisher relies
// on that).
func (s *Source) mismatch(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub, ok := s.publisher[u]
	if !ok {
		return
	}
	now := s.cfg.Now()
	for k, until := range s.demoted {
		if !until.After(now) {
			delete(s.demoted, k)
		}
	}
	if _, exists := s.demoted[pub]; !exists && len(s.demoted) >= maxDemoted {
		var oldest string
		var earliest time.Time
		for k, until := range s.demoted {
			if oldest == "" || until.Before(earliest) {
				oldest, earliest = k, until
			}
		}
		delete(s.demoted, oldest)
	}
	s.demoted[pub] = now.Add(s.cfg.Demotion)
}

// Demoted reports whether publisher is currently tried last.
func (s *Source) Demoted(publisher string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demoted[publisher].After(s.cfg.Now())
}

// FetchObject looks c up on the indexers and makes it held, verified, in the store from
// one of the located URLs (rasl.Retriever's race). Fresh candidates are tried first,
// demoted ones only if none of them served the object. With no usable location the
// error wraps ErrNoLocation; with no indexer answering it is a LookupError.
func (s *Source) FetchObject(ctx context.Context, c cid.CID, size uint64) error {
	locs, err := s.Lookup(ctx, c)
	if err != nil {
		return err
	}
	fresh, demoted := s.Candidates(locs)
	if len(fresh) == 0 && len(demoted) == 0 {
		return fmt.Errorf("%w for %s", ErrNoLocation, c)
	}
	se := &rasl.SourceError{CID: c}
	for _, round := range [][]string{fresh, demoted} {
		if len(round) == 0 {
			continue
		}
		err := s.r.Fetch(ctx, c, size, round)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var rse *rasl.SourceError
		if !errors.As(err, &rse) {
			return err
		}
		se.Attempts = append(se.Attempts, rse.Attempts...)
	}
	return se
}
