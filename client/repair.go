package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var _ node.RepairNode = (*Client)(nil)

// ClusterMemberSet edits a member record.
func (c *Client) ClusterMemberSet(ctx context.Context, req apiv0.MemberSetRequest) (*apiv0.MemberResult, error) {
	var out apiv0.MemberResult
	if _, err := c.do(ctx, http.MethodPost, "/cluster/members/"+url.PathEscape(req.Peer), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterKey creates a key on the daemon's node or grants a role to a DID.
func (c *Client) ClusterKey(ctx context.Context, req apiv0.KeyRequest) (*apiv0.KeyResult, error) {
	var out apiv0.KeyResult
	if _, err := c.do(ctx, http.MethodPost, "/cluster/keys", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterKeyRevoke revokes a key.
func (c *Client) ClusterKeyRevoke(ctx context.Context, did string) (*apiv0.KeyResult, error) {
	var out apiv0.KeyResult
	if _, err := c.do(ctx, http.MethodDelete, "/cluster/keys/"+url.PathEscape(did), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterInviteRevoke revokes an outstanding secret invite.
func (c *Client) ClusterInviteRevoke(ctx context.Context, id string) (*apiv0.MetadataState, error) {
	var out apiv0.MetadataState
	if _, err := c.do(ctx, http.MethodDelete, "/cluster/invites/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatasetRelease reports or applies safe release of a dataset on the daemon's node.
func (c *Client) DatasetRelease(ctx context.Context, scope, name string, req apiv0.ReleaseRequest) (*apiv0.ReleaseReport, error) {
	var out apiv0.ReleaseReport
	if _, err := c.do(ctx, http.MethodPost, historyPath(scope, name, "release"), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClusterAudit runs audit challenges and a scrub now.
func (c *Client) ClusterAudit(ctx context.Context, req apiv0.AuditRequest) (*apiv0.AuditReport, error) {
	var out apiv0.AuditReport
	if _, err := c.do(ctx, http.MethodPost, "/cluster/audit", nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
