package httpsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxURLBytes bounds URL parsing; response headers have a separate 64 KiB limit.
const MaxURLBytes = 8192

var (
	ErrPolicy   = errors.New("httpsrc: destination denied")
	ErrTooLarge = errors.New("httpsrc: size limit exceeded")
	ErrResponse = errors.New("httpsrc: invalid response")
)

// StatusError is an unexpected HTTP status; it matches ErrResponse with errors.Is.
type StatusError struct{ Code int }

func (e *StatusError) Error() string        { return fmt.Sprintf("%v: HTTP status %d", ErrResponse, e.Code) }
func (e *StatusError) Is(target error) bool { return target == ErrResponse }

// Config is trusted application policy, not input supplied by a remote caller.
type Config struct {
	AllowHTTP bool
	// AllowedIPs explicitly exempts address ranges from the default SSRF policy.
	AllowedIPs []netip.Prefix
	// MaxRedirects defaults to 5; negative values forbid redirects.
	MaxRedirects int
	// LookupIP defaults to net.DefaultResolver.LookupNetIP. Results are checked at
	// every dial, then dialed as literals, with no second DNS lookup.
	LookupIP func(context.Context, string) ([]netip.Addr, error)
	// DialContext is an optional trusted connector/test hook. It receives only
	// checked IP:port literals and must connect to that address, without resolving
	// or substituting another destination. Nil uses net.Dialer.
	DialContext func(context.Context, string, string) (net.Conn, error)
}

// Client is safe for concurrent use. It holds only policy and a bounded, ephemeral
// origin-demotion scoreboard, not cookies, proxies or cached connections.
type Client struct {
	cfg      Config
	mu       sync.Mutex
	failures map[string]time.Time
}

func New(cfg Config) *Client {
	cfg.AllowedIPs = append([]netip.Prefix(nil), cfg.AllowedIPs...)
	if cfg.MaxRedirects == 0 {
		cfg.MaxRedirects = 5
	}
	if cfg.LookupIP == nil {
		cfg.LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if cfg.DialContext == nil {
		cfg.DialContext = (&net.Dialer{}).DialContext
	}
	return &Client{cfg: cfg, failures: make(map[string]time.Time)}
}

// Options requires explicit positive byte and time limits. MaxBytes includes any
// resumed prefix. Nil Hosts permits all hosts; non-nil empty Hosts denies all.
// Hosts are exact DNS names or IP literals (no ports, suffixes or wildcards).
type Options struct {
	MaxBytes    int64
	Timeout     time.Duration
	IdleTimeout time.Duration
	Hosts       []string
	// Resume describes a retained prefix. Fetch its FinalURL to resume; a different
	// URL or any redirect restarts. On a body-copy error Result.Size is the
	// retained length. Errors before opening the sink leave the old prefix intact.
	Resume *Result
}

// Result is provenance, including on a body-copy error. Validators are untrusted
// HTTP metadata, not a content-integrity guarantee. Size includes a resumed prefix.
type Result struct {
	FinalURL     string
	ETag         string
	LastModified string
	Date         string
	ContentType  string
	Size         int64
}

// Sink is called once after response validation, before streaming. At offset 0
// it MUST discard any previous bytes/hash state; otherwise it MUST append to the
// retained prefix of exactly offset bytes. Returning an error rejects the response.
// The caller owns the writer and its lifecycle. Writes must return promptly or
// honor the caller's context: Go cannot interrupt an arbitrary blocked Writer.
// A non-resuming store import can use an io.Pipe to feed Store.AddBlob(r, -1),
// closing the pipe with Fetch's error so failed transfers cannot commit.
type Sink func(offset int64) (io.Writer, error)

// Fetch performs one GET, streaming without whole-body buffering. It never retries
// automatically. The caller must not publish partially written bytes on error.
func (c *Client) Fetch(ctx context.Context, rawURL string, sink Sink, opts Options) (Result, error) {
	var result Result
	if opts.MaxBytes <= 0 || opts.Timeout <= 0 || opts.IdleTimeout <= 0 || sink == nil {
		return result, errors.New("httpsrc: positive size/time limits and a sink are required")
	}
	if len(rawURL) > MaxURLBytes {
		return result, fmt.Errorf("%w: URL too long", ErrPolicy)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return result, err
	}
	if err = c.checkURL(u, opts.Hosts); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	tr := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		MaxResponseHeaderBytes: 64 << 10,
		TLSHandshakeTimeout:    opts.IdleTimeout, ResponseHeaderTimeout: opts.IdleTimeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return c.dial(ctx, network, addr, opts.IdleTimeout)
		},
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if err := c.checkURL(req.URL, opts.Hosts); err != nil {
			return err
		}
		if len(via) > c.cfg.MaxRedirects {
			return fmt.Errorf("%w: too many redirects", ErrPolicy)
		}
		// A validator is scoped to its resource, never to a redirect destination.
		req.Header.Del("Range")
		req.Header.Del("If-Range")
		req.Header.Del("Referer") // source URLs may contain private query tokens
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	var offset int64
	if r := opts.Resume; r != nil && r.Size > 0 && r.FinalURL == u.String() {
		if v := r.validator(); v != "" {
			offset = r.Size
			if offset > opts.MaxBytes {
				return result, ErrTooLarge
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			req.Header.Set("If-Range", v)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	result = Result{FinalURL: resp.Request.URL.String(), ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"), Date: resp.Header.Get("Date"),
		ContentType: resp.Header.Get("Content-Type")}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return result, fmt.Errorf("%w: encoded body", ErrResponse)
	}
	expected := resp.ContentLength
	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0 // no validator, changed resource, redirect, or ignored Range
	case http.StatusPartialContent:
		if offset == 0 || resp.Request.Header.Get("If-Range") == "" {
			return result, fmt.Errorf("%w: unsolicited range", ErrResponse)
		}
		start, end, total, ok := contentRange(resp.Header.Get("Content-Range"), 128)
		if !ok || start != offset || end != total-1 || (expected >= 0 && expected != total-start) {
			return result, fmt.Errorf("%w: inconsistent range", ErrResponse)
		}
		if total > opts.MaxBytes {
			return result, ErrTooLarge
		}
		// A 206 confirms If-Range even if the server omits validators. Reject any
		// explicitly contradictory validator and preserve the saved metadata.
		old := opts.Resume
		if (result.ETag != "" && old.ETag != "" && result.ETag != old.ETag) ||
			(old.validator() == old.LastModified && result.LastModified != "" && result.LastModified != old.LastModified) {
			return result, fmt.Errorf("%w: changed range validator", ErrResponse)
		}
		if result.ETag == "" {
			result.ETag = old.ETag
		}
		if result.LastModified == "" {
			result.LastModified = old.LastModified
		}
		if result.Date == "" {
			result.Date = old.Date
		}
		expected = total - start
	default:
		return result, &StatusError{Code: resp.StatusCode}
	}
	if expected > opts.MaxBytes-offset {
		return result, ErrTooLarge
	}
	dst, err := sink(offset)
	if err != nil {
		return result, err
	}
	if dst == nil {
		return result, errors.New("httpsrc: nil sink writer")
	}
	result.Size = offset
	// Copy at most the budget, then probe one byte without writing past the limit.
	n, err := io.Copy(dst, io.LimitReader(resp.Body, opts.MaxBytes-offset))
	result.Size += n
	if err == nil && n == opts.MaxBytes-offset {
		var b [1]byte
		var read int
		read, err = io.ReadFull(resp.Body, b[:])
		if read > 0 {
			err = ErrTooLarge
		} else if err == io.EOF {
			err = nil
		}
	}
	if err == nil && expected >= 0 && n != expected {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = ctx.Err()
	}
	return result, err
}

func (r Result) validator() string {
	if strongETag(r.ETag) {
		return r.ETag
	}
	// RFC 9110 13.1.5: dates may be used for If-Range only with no entity tag.
	// RFC 9110 8.8.2.2's client rule requires Date >= Last-Modified + 60 s.
	if r.ETag != "" {
		return ""
	}
	lm, e1 := http.ParseTime(r.LastModified)
	date, e2 := http.ParseTime(r.Date)
	if e1 == nil && e2 == nil && date.Sub(lm) >= time.Minute {
		return r.LastModified
	}
	return ""
}

func strongETag(s string) bool {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '"' || s[i] < 0x21 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func contentRange(s string, maxBytes int) (start, end, total int64, ok bool) {
	if len(s) > maxBytes || !strings.HasPrefix(s, "bytes ") {
		return
	}
	span, count, found := strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	if !found {
		return
	}
	lo, hi, found := strings.Cut(span, "-")
	if !found {
		return
	}
	parse := func(s string) (int64, error) {
		if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, ErrResponse
		}
		return strconv.ParseInt(s, 10, 64)
	}
	var a, b, c error
	start, a = parse(lo)
	end, b = parse(hi)
	total, c = parse(count)
	ok = a == nil && b == nil && c == nil && start >= 0 && end >= start && total > end
	return
}
