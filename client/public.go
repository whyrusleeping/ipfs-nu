package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// node.PublicNode over HTTP (0043).

var _ node.PublicNode = (*Client)(nil)

// PublicAdd marks a root public.
func (c *Client) PublicAdd(ctx context.Context, req apiv0.PublicRequest) (*apiv0.PublicRoot, error) {
	var root apiv0.PublicRoot
	if _, err := c.do(ctx, http.MethodPost, "/public", nil, &req, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

// PublicRm removes a public mark.
func (c *Client) PublicRm(ctx context.Context, cid string) error {
	res, err := c.do(ctx, http.MethodDelete, "/public/"+url.PathEscape(cid), nil, nil, nil)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// PublicList lists public roots.
func (c *Client) PublicList(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.PublicRoot], error) {
	var page apiv0.Page[apiv0.PublicRoot]
	if _, err := c.do(ctx, http.MethodGet, "/public", pageQuery(cursor, limit), nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}
