package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var _ node.ClusterNode = (*Client)(nil)

// ClusterInit founds a networked cluster.
func (c *Client) ClusterInit(ctx context.Context, req apiv0.ClusterInitRequest) (*apiv0.ClusterInfo, error) {
	var out apiv0.ClusterInfo
	if _, err := c.do(ctx, http.MethodPost, "/cluster/init", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterInvite creates an invite token.
func (c *Client) ClusterInvite(ctx context.Context, req apiv0.ClusterInviteRequest) (*apiv0.ClusterInvite, error) {
	var out apiv0.ClusterInvite
	if _, err := c.do(ctx, http.MethodPost, "/cluster/invites", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterJoin joins a cluster with a token.
func (c *Client) ClusterJoin(ctx context.Context, req apiv0.ClusterJoinRequest) (*apiv0.ClusterInfo, error) {
	var out apiv0.ClusterInfo
	if _, err := c.do(ctx, http.MethodPost, "/cluster/join", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterStatus describes the node's clusters.
func (c *Client) ClusterStatus(ctx context.Context) (*apiv0.ClusterStatus, error) {
	var out apiv0.ClusterStatus
	if _, err := c.do(ctx, http.MethodGet, "/cluster", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterMemberRemove removes a member.
func (c *Client) ClusterMemberRemove(ctx context.Context, req apiv0.ClusterMemberRemoveRequest) (*apiv0.MetadataState, error) {
	var out apiv0.MetadataState
	if _, err := c.do(ctx, http.MethodDelete, "/cluster/members/"+url.PathEscape(req.Peer), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func datasetsPath(scope string) string {
	if scope == node.ScopeCluster {
		return "/cluster/datasets"
	}
	return "/datasets"
}

// DatasetCreate creates a dataset in the local or networked cluster.
func (c *Client) DatasetCreate(ctx context.Context, scope string, req apiv0.DatasetCreateRequest) (*apiv0.Dataset, error) {
	var out apiv0.Dataset
	if _, err := c.do(ctx, http.MethodPost, datasetsPath(scope), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Datasets lists datasets.
func (c *Client) Datasets(ctx context.Context, scope string) ([]apiv0.Dataset, error) {
	var out []apiv0.Dataset
	if _, err := c.do(ctx, http.MethodGet, datasetsPath(scope), nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Dataset returns one dataset.
func (c *Client) Dataset(ctx context.Context, scope, name string) (*apiv0.Dataset, error) {
	var out apiv0.Dataset
	if _, err := c.do(ctx, http.MethodGet, datasetsPath(scope)+"/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
