package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var _ node.HistoryNode = (*Client)(nil)

func historyPath(scope, name, op string) string {
	return datasetsPath(scope) + "/" + url.PathEscape(name) + "/" + op
}

// DatasetVersions lists a dataset's versions with their availability.
func (c *Client) DatasetVersions(ctx context.Context, scope, name string) (*apiv0.DatasetVersions, error) {
	var out apiv0.DatasetVersions
	if _, err := c.do(ctx, http.MethodGet, historyPath(scope, name, "versions"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatasetLabel labels (or unlabels) a version.
func (c *Client) DatasetLabel(ctx context.Context, scope, name string, req apiv0.VersionLabelRequest) (*apiv0.VersionLabel, error) {
	var out apiv0.VersionLabel
	if _, err := c.do(ctx, http.MethodPost, historyPath(scope, name, "label"), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatasetRollback writes a new head restoring a version.
func (c *Client) DatasetRollback(ctx context.Context, scope, name string, req apiv0.RollbackRequest) (*apiv0.Dataset, error) {
	var out apiv0.Dataset
	if _, err := c.do(ctx, http.MethodPost, historyPath(scope, name, "rollback"), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatasetDiff lists the paths that differ between two versions.
func (c *Client) DatasetDiff(ctx context.Context, scope, name, from, to string) (*apiv0.DatasetDiff, error) {
	var out apiv0.DatasetDiff
	if _, err := c.do(ctx, http.MethodGet, historyPath(scope, name, "diff"), url.Values{"from": {from}, "to": {to}}, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatasetPrune prunes (or with DryRun reports) versions outside the live set.
func (c *Client) DatasetPrune(ctx context.Context, scope, name string, req apiv0.PruneRequest) (*apiv0.PruneReport, error) {
	var out apiv0.PruneReport
	if _, err := c.do(ctx, http.MethodPost, historyPath(scope, name, "prune"), nil, &req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
