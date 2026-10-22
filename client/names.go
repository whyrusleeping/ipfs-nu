package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// node.NamesNode over HTTP (0045).

var _ node.NamesNode = (*Client)(nil)

func (c *Client) Login(ctx context.Context, req apiv0.LoginRequest) (*apiv0.LoginStart, error) {
	var out apiv0.LoginStart
	if _, err := c.do(ctx, http.MethodPost, "/auth/login", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Logout(ctx context.Context, account string) error {
	if account == "" {
		account = "-"
	}
	res, err := c.do(ctx, http.MethodDelete, "/auth/accounts/"+url.PathEscape(account), nil, nil, nil)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func (c *Client) Accounts(ctx context.Context) ([]apiv0.Account, error) {
	var out []apiv0.Account
	if _, err := c.do(ctx, http.MethodGet, "/auth/accounts", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Publish(ctx context.Context, req apiv0.PublishRequest) (*apiv0.PublishResult, error) {
	var out apiv0.PublishResult
	if _, err := c.do(ctx, http.MethodPost, "/names", nil, &req, &out); err != nil {
		// A failed or timed-out seed wait still carries the publication in details.result.
		var ae *apiv0.Error
		if errors.As(err, &ae) {
			if raw, ok := ae.Details["result"]; ok {
				if b, merr := json.Marshal(raw); merr == nil && json.Unmarshal(b, &out) == nil {
					return &out, err
				}
			}
		}
		return nil, err
	}
	return &out, nil
}

func (c *Client) Unpublish(ctx context.Context, name, account string) (*apiv0.Publication, error) {
	var out apiv0.Publication
	if _, err := c.do(ctx, http.MethodDelete, "/names/"+url.PathEscape(name), accountQuery(account), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func accountQuery(account string) url.Values {
	if account == "" {
		return nil
	}
	return url.Values{"account": {account}}
}

func (c *Client) ResolveName(ctx context.Context, target string, req apiv0.ResolveRequest) (*apiv0.NameResolution, error) {
	q := url.Values{"target": {target}}
	if req.MaxAge != "" {
		q.Set("max_age", req.MaxAge)
	}
	if req.Fresh {
		q.Set("fresh", "1")
	}
	if req.Offline {
		q.Set("offline", "1")
	}
	var out apiv0.NameResolution
	if _, err := c.do(ctx, http.MethodGet, "/resolve", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Names(ctx context.Context, account string) (*apiv0.NameList, error) {
	var out apiv0.NameList
	if _, err := c.do(ctx, http.MethodGet, "/names", accountQuery(account), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) NodeRegister(ctx context.Context, req apiv0.NodeRegisterRequest) (*apiv0.NodeRecord, error) {
	var out apiv0.NodeRecord
	if _, err := c.do(ctx, http.MethodPost, "/node/register", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Devices(ctx context.Context, account string) (*apiv0.DeviceList, error) {
	var out apiv0.DeviceList
	if _, err := c.do(ctx, http.MethodGet, "/node/devices", accountQuery(account), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Follow(ctx context.Context, req apiv0.FollowRequest) (*apiv0.Follow, error) {
	var out apiv0.Follow
	if _, err := c.do(ctx, http.MethodPost, "/follows", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Follows(ctx context.Context) ([]apiv0.Follow, error) {
	var out []apiv0.Follow
	if _, err := c.do(ctx, http.MethodGet, "/follows", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Unfollow(ctx context.Context, dataset string) (*apiv0.Follow, error) {
	var out apiv0.Follow
	if _, err := c.do(ctx, http.MethodDelete, "/follows/"+url.PathEscape(dataset), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) FollowRefresh(ctx context.Context, req apiv0.FollowRefreshRequest) ([]apiv0.Follow, error) {
	var out []apiv0.Follow
	if _, err := c.do(ctx, http.MethodPost, "/follows:refresh", nil, &req, &out); err != nil {
		return nil, err
	}
	return out, nil
}
