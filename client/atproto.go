package client

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// node.AtprotoNode over HTTP (0053).

var _ node.AtprotoNode = (*Client)(nil)

// AtRecord fetches a verified record.
func (c *Client) AtRecord(ctx context.Context, target string) (*apiv0.AtRecord, error) {
	var rec apiv0.AtRecord
	if _, err := c.do(ctx, http.MethodGet, "/at/record", url.Values{"target": {target}}, nil, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// AtSnapshot starts a snapshot operation.
func (c *Client) AtSnapshot(ctx context.Context, req apiv0.AtSnapshotRequest) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodPost, "/at/snapshot", nil, &req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// CarImport starts a CAR import operation.
func (c *Client) CarImport(ctx context.Context, req apiv0.CarImportRequest) (*apiv0.Operation, error) {
	var o apiv0.Operation
	if _, err := c.do(ctx, http.MethodPost, "/car/import", nil, &req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// CarExport streams a CAR export.
func (c *Client) CarExport(ctx context.Context, target, profile string) (io.ReadCloser, error) {
	q := url.Values{"target": {target}}
	if profile != "" {
		q.Set("profile", profile)
	}
	res, err := c.do(ctx, http.MethodGet, "/car", q, nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
