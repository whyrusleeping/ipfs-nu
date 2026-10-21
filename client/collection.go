package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

var _ node.CollectionNode = (*Client)(nil)

func collPath(name string) string { return "/cluster/collections/" + url.PathEscape(name) }

// CollectionStatus summarises a collection on the daemon's node.
func (c *Client) CollectionStatus(ctx context.Context, name string) (*apiv0.CollectionStatus, error) {
	var out apiv0.CollectionStatus
	if _, err := c.do(ctx, http.MethodGet, collPath(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionItems queries items or follows the item change stream.
func (c *Client) CollectionItems(ctx context.Context, name string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error) {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("since", q.Since)
	set("cursor", q.Cursor)
	set("holder", q.Holder)
	set("prefix", q.Prefix)
	set("match", string(q.Match))
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	for k, b := range map[string]bool{"rarest": q.Rarest, "unheld": q.Unheld, "held": q.Held} {
		if b {
			v.Set(k, "true")
		}
	}
	var out apiv0.Page[apiv0.CollectionItem]
	if _, err := c.do(ctx, http.MethodGet, collPath(name)+"/items", v, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionItem describes one item with its holders and conflicts.
func (c *Client) CollectionItem(ctx context.Context, name, path string) (*apiv0.CollectionItem, error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	var out apiv0.CollectionItem
	if _, err := c.do(ctx, http.MethodGet, collPath(name)+"/items/"+strings.Join(segs, "/"), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionRules returns the node's selection rules and their current impact.
func (c *Client) CollectionRules(ctx context.Context, name string) (*apiv0.CollectionRules, error) {
	var out apiv0.CollectionRules
	if _, err := c.do(ctx, http.MethodGet, collPath(name)+"/rules", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionSetRules replaces (or with dryRun evaluates) the node's selection rules.
func (c *Client) CollectionSetRules(ctx context.Context, name string, rules json.RawMessage, dryRun bool) (*apiv0.CollectionRules, error) {
	var q url.Values
	if dryRun {
		q = url.Values{"dry_run": {"true"}}
	}
	if len(rules) == 0 {
		rules = json.RawMessage("[]")
	}
	var out apiv0.CollectionRules
	if _, err := c.do(ctx, http.MethodPut, collPath(name)+"/rules", q, map[string]json.RawMessage{"rules": rules}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionHold sets (hold) or clears a manual hold.
func (c *Client) CollectionHold(ctx context.Context, name, path string, hold bool) (*apiv0.CollectionItem, error) {
	verb := "/release"
	if hold {
		verb = "/hold"
	}
	var out apiv0.CollectionItem
	if _, err := c.do(ctx, http.MethodPost, collPath(name)+verb, nil, &apiv0.CollectionHoldRequest{Path: path}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CollectionConflicts lists union-merge conflicts.
func (c *Client) CollectionConflicts(ctx context.Context, name string) ([]apiv0.CollectionConflict, error) {
	var out []apiv0.CollectionConflict
	if _, err := c.do(ctx, http.MethodGet, collPath(name)+"/conflicts", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
