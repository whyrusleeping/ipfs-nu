package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var _ node.PushNode = (*Client)(nil)

func itemPath(name, path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return collPath(name) + "/items/" + strings.Join(segs, "/")
}

func waitQuery(wait time.Duration) url.Values {
	if wait <= 0 {
		return nil
	}
	return url.Values{"wait": {apiv0.PushWritten}, "timeout": {wait.String()}}
}

// CollectionPush pushes an item; with wait > 0 the daemon waits (at most
// apiv0.MaxPushWait) until it is written or failed.
func (c *Client) CollectionPush(ctx context.Context, name string, req apiv0.CollectionPushRequest, wait time.Duration) (*apiv0.CollectionPush, error) {
	var out apiv0.CollectionPush
	if _, err := c.do(ctx, http.MethodPost, collPath(name)+"/items", waitQuery(min(wait, apiv0.MaxPushWait)), &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionPushBatch sends pushes as one NDJSON batch.
func (c *Client) CollectionPushBatch(ctx context.Context, name string, reqs []apiv0.CollectionPushRequest) ([]apiv0.CollectionPushBatchLine, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range reqs {
		if err := enc.Encode(&reqs[i]); err != nil {
			return nil, err
		}
	}
	res, err := c.send(ctx, http.MethodPost, collPath(name)+"/items:batch", nil, "application/x-ndjson", &body, -1, nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out []apiv0.CollectionPushBatchLine
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l apiv0.CollectionPushBatchLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "malformed batch response: %v", err)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// CollectionRemove removes an item from the node's writer head.
func (c *Client) CollectionRemove(ctx context.Context, name, path string, wait time.Duration) (*apiv0.CollectionPush, error) {
	var out apiv0.CollectionPush
	if _, err := c.do(ctx, http.MethodDelete, itemPath(name, path), waitQuery(min(wait, apiv0.MaxPushWait)), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionPushes follows the push log.
func (c *Client) CollectionPushes(ctx context.Context, name, since string, limit int) (*apiv0.Page[apiv0.CollectionPush], error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out apiv0.Page[apiv0.CollectionPush]
	if _, err := c.do(ctx, http.MethodGet, collPath(name)+"/pushes", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionUpload streams a file to the node for a later push.
func (c *Client) CollectionUpload(ctx context.Context, name string, r io.Reader, size int64) (*apiv0.CollectionUpload, error) {
	var out apiv0.CollectionUpload
	if _, err := c.send(ctx, http.MethodPost, collPath(name)+"/uploads", nil, "application/octet-stream", r, size, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
