// Package client is the Go client for the local API; the CLI is built only on it.
//
// Client implements node.Node over HTTP. API errors decode into *apiv0.Error (alias
// Error) carrying the stable code, message and action; connection failures become code
// unavailable with the action to start the daemon.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/repo"
)

// Error is the typed API error.
type Error = apiv0.Error

// Client talks to one daemon.
type Client struct {
	base  string // e.g. http://127.0.0.1:5171
	http  *http.Client
	token string // bearer token for a producer listener (A8), or ""
}

// WithToken returns a copy of c that sends token as its bearer credential (the push
// token of a remote node's producer listener).
func (c *Client) WithToken(token string) *Client {
	d := *c
	d.token = token
	return &d
}

// New returns a client for addr, given as host:port or an http:// URL.
func New(addr string) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Client{base: strings.TrimRight(addr, "/"), http: &http.Client{}}
}

// Discover returns a client for the daemon recorded in the repo's API file. When no
// daemon has written one it returns an unavailable error; it does not contact the daemon.
func Discover(r *repo.Repo) (*Client, error) {
	addr, err := r.ReadAddrFile(repo.APIFile)
	if err != nil {
		return nil, err
	}
	if addr == "" {
		return nil, noDaemon(fmt.Sprintf("no daemon is running for %s", r.Root()))
	}
	return New(addr), nil
}

// Base returns the API base URL.
func (c *Client) Base() string { return c.base }

func noDaemon(msg string) *apiv0.Error {
	return &apiv0.Error{
		Code:    apiv0.CodeUnavailable,
		Message: msg,
		Action:  "start it with `" + nu.Name + " daemon`",
	}
}

func (c *Client) url(path string, q url.Values) string {
	u := c.base + apiv0.Prefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do performs a JSON request. body (if non-nil) is JSON-encoded; out (if non-nil) decodes
// the response. The response is closed unless raw is true, in which case it is returned.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) (*http.Response, error) {
	var rd io.Reader
	ctype := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd, ctype = bytes.NewReader(b), "application/json"
	}
	return c.send(ctx, method, path, q, ctype, rd, -1, out)
}

// send is do with a raw body of the given type (size -1 when unknown).
func (c *Client) send(ctx context.Context, method, path string, q url.Values, ctype string, rd io.Reader, size int64, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rd)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, noDaemon(fmt.Sprintf("cannot reach the daemon at %s: %v", c.base, err))
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		return nil, decodeError(res)
	}
	if out == nil {
		return res, nil
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "malformed response from the daemon: %v", err)
	}
	return nil, nil
}

func decodeError(res *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var env apiv0.Envelope
	if err := json.Unmarshal(b, &env); err == nil && env.Error != nil && env.Error.Code != "" {
		return env.Error
	}
	return apiv0.Errorf(apiv0.CodeFailed, "daemon answered %s without an error envelope", res.Status)
}

// Status returns the daemon summary.
func (c *Client) Status(ctx context.Context) (*apiv0.Status, error) {
	var st apiv0.Status
	if _, err := c.do(ctx, http.MethodGet, "/status", nil, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// OpenAPI fetches the API description.
func (c *Client) OpenAPI(ctx context.Context) ([]byte, error) {
	res, err := c.do(ctx, http.MethodGet, "/openapi.json", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func pageQuery(cursor string, limit int) url.Values {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q
}

// Operations lists retained operations.
func (c *Client) Operations(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Operation], error) {
	var page apiv0.Page[apiv0.Operation]
	if _, err := c.do(ctx, http.MethodGet, "/operations", pageQuery(cursor, limit), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// Operation gets one operation.
func (c *Client) Operation(ctx context.Context, id string) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id), nil, nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// OperationEvents streams events after seq since. The stream lives as long as ctx.
func (c *Client) OperationEvents(ctx context.Context, id string, since int64) (apiv0.EventStream, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	res, err := c.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id)+"/events", q, nil, nil)
	if err != nil {
		return nil, err
	}
	return &EventStream{body: res.Body, dec: json.NewDecoder(res.Body)}, nil
}

// EventStream reads NDJSON operation events.
type EventStream struct {
	body  io.ReadCloser
	dec   *json.Decoder
	ended bool
}

// Next returns the next event, or io.EOF after the done event.
func (s *EventStream) Next(ctx context.Context) (*apiv0.Event, error) {
	if s.ended {
		return nil, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var ev apiv0.Event
	if err := s.dec.Decode(&ev); err != nil {
		if errors.Is(err, io.EOF) {
			// The daemon closed the stream before the done event (shutdown).
			return nil, noDaemon("the daemon closed the event stream")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apiv0.Errorf(apiv0.CodeFailed, "malformed event from the daemon: %v", err)
	}
	if ev.Type == apiv0.EventDone {
		s.ended = true
	}
	return &ev, nil
}

// Close closes the stream.
func (s *EventStream) Close() error { return s.body.Close() }

// WaitOperation waits until the operation finishes or timeout elapses (0: as long as ctx
// lives). It loops over server-side waits, so long timeouts are fine. On expiry it returns
// a timeout error whose details hold the latest snapshot.
func (c *Client) WaitOperation(ctx context.Context, id string, timeout time.Duration) (*apiv0.Operation, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		per := 5 * time.Minute
		if !deadline.IsZero() {
			if rem := time.Until(deadline); rem < per {
				per = max(rem, time.Millisecond)
			}
		}
		q := url.Values{"timeout": {per.String()}}
		var o apiv0.Operation
		_, err := c.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id)+"/wait", q, nil, &o)
		if err == nil {
			return &o, nil
		}
		var ae *apiv0.Error
		if errors.As(err, &ae) && ae.Code == apiv0.CodeTimeout && (deadline.IsZero() || time.Now().Before(deadline)) {
			continue
		}
		return nil, err
	}
}

// CancelOperation cancels a running operation.
func (c *Client) CancelOperation(ctx context.Context, id string) error {
	var o apiv0.Operation
	_, err := c.do(ctx, http.MethodPost, "/operations/"+url.PathEscape(id)+"/cancel", nil, nil, &o)
	return err
}

// Add starts an add operation.
func (c *Client) Add(ctx context.Context, req apiv0.AddRequest) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodPost, "/add", nil, &req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// Cat streams a target's bytes.
func (c *Client) Cat(ctx context.Context, target string) (io.ReadCloser, error) {
	res, err := c.do(ctx, http.MethodGet, "/cat", url.Values{"target": {target}}, nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Ls lists a directory target.
func (c *Client) Ls(ctx context.Context, target string) (*apiv0.Listing, error) {
	var l apiv0.Listing
	if _, err := c.do(ctx, http.MethodGet, "/ls", url.Values{"target": {target}}, nil, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// PinAdd starts a pin operation.
func (c *Client) PinAdd(ctx context.Context, req apiv0.PinRequest) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodPost, "/pins", nil, &req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// PinRm removes a pin.
func (c *Client) PinRm(ctx context.Context, cid string) error {
	res, err := c.do(ctx, http.MethodDelete, "/pins/"+url.PathEscape(cid), nil, nil, nil)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// Pins lists pins.
func (c *Client) Pins(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Pin], error) {
	var page apiv0.Page[apiv0.Pin]
	if _, err := c.do(ctx, http.MethodGet, "/pins", pageQuery(cursor, limit), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// DagGet returns a DRISL object as JSON.
func (c *Client) DagGet(ctx context.Context, target string) (json.RawMessage, error) {
	res, err := c.do(ctx, http.MethodGet, "/dag", url.Values{"target": {target}}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

// DagPut stores one DRISL object given as JSON.
func (c *Client) DagPut(ctx context.Context, doc io.Reader) (*apiv0.DagPutResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/dag", nil), doc)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, noDaemon(fmt.Sprintf("cannot reach the daemon at %s: %v", c.base, err))
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, decodeError(res)
	}
	var out apiv0.DagPutResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "malformed response from the daemon: %v", err)
	}
	return &out, nil
}

// Get starts a get operation.
func (c *Client) Get(ctx context.Context, req apiv0.GetRequest) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodPost, "/get", nil, &req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// Share returns the share link of held content.
func (c *Client) Share(ctx context.Context, req apiv0.ShareRequest) (*apiv0.ShareLink, error) {
	var res apiv0.ShareLink
	if _, err := c.do(ctx, http.MethodPost, "/share", nil, &req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
