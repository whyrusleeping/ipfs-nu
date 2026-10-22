package node

import (
	"context"
	"net/url"
	"strings"
	"sync"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/rasl"
	"github.com/whyrusleeping/nu/store"
)

// Public roots (backlog 0043; docs/operations.md P2). The operator marks roots public
// one by one; the mark is a pin with store.SourcePublic, so public content also survives
// GC. While the daemon runs the public listener, the node keeps an in-memory set of every
// object reachable from the public roots (rebuilt from the pins at start, extended on
// add, rebuilt on remove) and the listener serves exactly that set: trees, blobs and
// RASL objects under a public root, nothing else, with no on-demand fetching (P4).

// PublicNode marks roots for the public listener; Local implements it, the client
// mirrors it over the API.
type PublicNode interface {
	// PublicAdd marks a held, complete root public.
	PublicAdd(ctx context.Context, req apiv0.PublicRequest) (*apiv0.PublicRoot, error)
	// PublicRm removes the mark.
	PublicRm(ctx context.Context, cid string) error
	// PublicList lists the public roots, paged by opaque cursor.
	PublicList(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.PublicRoot], error)
}

var _ PublicNode = (*Local)(nil)

// publicState is the node's public-serving state.
type publicState struct {
	mu sync.RWMutex
	// domain is the public listener's domain ("" when none runs in this process).
	domain string
	// objects is every object under a public root, keyed by full CID (codec included:
	// a raw file's multihash under the DRISL codec is a different object, 0077 N42); nil
	// until EnablePublic.
	objects map[cid.CID]struct{}
}

func publicPin(c cid.CID) store.Pin {
	p := pinFor(c, "")
	p.Source = store.SourcePublic
	return p
}

// EnablePublic prepares the node to serve public roots on the listener for domain
// (already validated by gateway.ParseDomain) and returns the listener's options. It
// walks the public roots once to build the served set.
func (n *Local) EnablePublic(ctx context.Context, domain string) (gateway.PublicOptions, error) {
	objects, err := n.walkPublic(ctx)
	if err != nil {
		return gateway.PublicOptions{}, err
	}
	ps := &n.public
	ps.mu.Lock()
	ps.domain, ps.objects = domain, objects
	ps.mu.Unlock()
	return gateway.PublicOptions{Domain: domain, Serves: n.PublicServes}, nil
}

// walkPublic collects every object under the public pins.
func (n *Local) walkPublic(ctx context.Context) (map[cid.CID]struct{}, error) {
	pins, err := n.Store.Pins()
	if err != nil {
		return nil, err
	}
	objects := map[cid.CID]struct{}{}
	for _, p := range pins {
		if p.Source != store.SourcePublic {
			continue
		}
		if _, err := n.Store.Walk(ctx, p, func(c cid.CID, held bool) error {
			if held {
				objects[c] = struct{}{}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// PublicServes reports whether the public listener may serve c: it is under a public
// root (as this exact CID, codec included) and the listener is enabled. It is gateway.PublicOptions.Serves.
func (n *Local) PublicServes(s string) bool {
	c, err := cid.Parse(s)
	if err != nil {
		return false
	}
	ps := &n.public
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.objects == nil {
		return false
	}
	_, ok := ps.objects[c]
	return ok
}

// PublicOrigin is the public listener's https origin, "" when none runs here.
func (n *Local) PublicOrigin() string {
	ps := &n.public
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.domain == "" {
		return ""
	}
	return (&gateway.PublicOptions{Domain: ps.domain}).Origin()
}

// publicLinks fills the HTTPS/Site/RASL links of a root (with optional path and its
// resolved kind) when this process's public listener serves it now (P2).
func (n *Local) publicLinks(root cid.CID, p string, kind gateway.Kind) (https, site, rsl string) {
	ps := &n.public
	ps.mu.RLock()
	domain := ps.domain
	ps.mu.RUnlock()
	if domain == "" || !n.PublicServes(root.String()) {
		return "", "", ""
	}
	o := &gateway.PublicOptions{Domain: domain}
	esc := escapePath(p)
	switch {
	case root.Codec() == cid.Raw:
		https = o.Origin() + "/blob/" + root.String()
	case kind == gateway.KindDir:
		https = o.Origin() + "/tree/" + root.String() + "/" + esc
		if esc != "" {
			https += "/"
		}
		site = o.SiteURL(root.String()) + esc
		if esc != "" {
			site += "/"
		}
	default:
		https = o.Origin() + "/tree/" + root.String()
		if esc != "" {
			https += "/" + esc
		}
	}
	rsl = rasl.URL{CID: root, Hints: []string{domain}}.String()
	return https, site, rsl
}

func escapePath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (n *Local) publicRoot(c cid.CID) *apiv0.PublicRoot {
	r := &apiv0.PublicRoot{CID: c.String()}
	kind := gateway.KindBlob
	if nd, err := n.content.Resolve(c, ""); err == nil {
		kind = nd.Kind
	}
	r.HTTPS, r.Site, r.RASL = n.publicLinks(c, "", kind)
	return r
}

// PublicAdd marks a held, complete root public (and pins it under the public source).
// When the listener runs, the root's objects are served as soon as this returns.
func (n *Local) PublicAdd(ctx context.Context, req apiv0.PublicRequest) (*apiv0.PublicRoot, error) {
	c, err := parseCID(req.CID)
	if err != nil {
		return nil, err
	}
	if err := n.redistributable(c); err != nil {
		return nil, err
	}
	if err := n.memberOnly(c); err != nil {
		return nil, err
	}
	if _, ok := n.atrepoRoot(c); ok {
		return nil, apiv0.Errorf(apiv0.CodeNotSupported, "%s is an atproto repository commit; the public listener has no /at/ route (P4)", c).
			WithAction("atproto archives are served to members and on the loopback gateway; public current-state serving of accounts is a later task")
	}
	if nd, err := n.content.Resolve(c, ""); err != nil {
		return nil, err
	} else if nd.Kind != gateway.KindDir && nd.Kind != gateway.KindFile && nd.Kind != gateway.KindBlob {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not a tree, file or blob", c)
	}
	p := publicPin(c)
	var objects []cid.CID
	st, err := n.Store.Walk(ctx, p, func(c cid.CID, held bool) error {
		if held {
			objects = append(objects, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !st.Complete() {
		return nil, missing(c, st.Missing, st.Objects)
	}
	if err := n.Store.Pin(p); err != nil {
		return nil, err
	}
	ps := &n.public
	ps.mu.Lock()
	if ps.objects != nil {
		for _, o := range objects {
			ps.objects[o] = struct{}{}
		}
	}
	ps.mu.Unlock()
	return n.publicRoot(c), nil
}

// PublicRm removes the public mark; the listener stops serving the root's objects
// (except those shared with another public root) before this returns.
func (n *Local) PublicRm(ctx context.Context, s string) error {
	c, err := parseCID(s)
	if err != nil {
		return err
	}
	ok, err := n.Store.Unpin(publicPin(c))
	if err != nil {
		return err
	}
	if !ok {
		return apiv0.Errorf(apiv0.CodeNotFound, "%s is not public", c).WithAction("list public roots with `" + nu.Name + " public ls`")
	}
	ps := &n.public
	ps.mu.RLock()
	enabled := ps.objects != nil
	ps.mu.RUnlock()
	if !enabled {
		return nil
	}
	objects, err := n.walkPublic(ctx)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	ps.objects = objects
	ps.mu.Unlock()
	return nil
}

// PublicList lists the public roots in marking order.
func (n *Local) PublicList(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.PublicRoot], error) {
	pins, err := n.Store.Pins()
	if err != nil {
		return nil, err
	}
	items := []apiv0.PublicRoot{}
	for _, p := range pins {
		if p.Source == store.SourcePublic {
			items = append(items, *n.publicRoot(p.CID))
		}
	}
	return apiv0.PageSlice(items, cursor, limit, n.Ops.Epoch())
}

// resolvedKind maps a resolved node to the kind publicLinks needs.
func resolvedKind(nd *content.Node) gateway.Kind {
	if nd == nil {
		return gateway.KindBlob
	}
	return nd.Kind
}
