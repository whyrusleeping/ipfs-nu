package httpsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Do performs one request under the same destination policy as Fetch (URL checks, IP
// policy at dial time, no redirects, bounded response headers and body) for callers
// that need request headers and response headers: the atproto OAuth and XRPC write
// paths (0045). The response body is already limited to opts.MaxBytes (reading past it
// fails with ErrTooLarge) and must be closed by the caller. opts.Resume is ignored.
// Any HTTP status is returned as a response, not an error.
func (c *Client) Do(ctx context.Context, req *http.Request, opts Options) (*http.Response, error) {
	if opts.MaxBytes <= 0 || opts.Timeout <= 0 || opts.IdleTimeout <= 0 {
		return nil, errors.New("httpsrc: positive size/time limits are required")
	}
	if req.URL == nil || len(req.URL.String()) > MaxURLBytes {
		return nil, fmt.Errorf("%w: URL missing or too long", ErrPolicy)
	}
	if err := c.checkURL(req.URL, opts.Hosts); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	tr := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		MaxResponseHeaderBytes: 64 << 10,
		TLSHandshakeTimeout:    opts.IdleTimeout, ResponseHeaderTimeout: opts.IdleTimeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return c.dial(ctx, network, addr, opts.IdleTimeout)
		},
	}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: redirects forbidden", ErrPolicy)
	}}
	req = req.Clone(ctx)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		tr.CloseIdleConnections()
		return nil, err
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%w: encoded body", ErrResponse)
	}
	if resp.ContentLength > opts.MaxBytes {
		resp.Body.Close()
		cancel()
		return nil, ErrTooLarge
	}
	resp.Body = &boundedBody{r: io.LimitReader(resp.Body, opts.MaxBytes+1), body: resp.Body, max: opts.MaxBytes, done: func() { cancel(); tr.CloseIdleConnections() }}
	return resp, nil
}

// boundedBody fails with ErrTooLarge once more than max bytes arrive.
type boundedBody struct {
	r    io.Reader
	body io.ReadCloser
	max  int64
	n    int64
	done func()
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return 0, ErrTooLarge
	}
	return n, err
}

func (b *boundedBody) Close() error {
	err := b.body.Close()
	b.done()
	return err
}

// ReadAll reads a response body under Do's limit and closes it.
func ReadAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
