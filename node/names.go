package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/follow"
	"github.com/whyrusleeping/nu/names"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// NamesNode is login, public names, node registration and following (0045;
// docs/names.md D4–D7, docs/follow.md). Named publication requires a login; resolution
// and following do not.
type NamesNode interface {
	// Login starts an OAuth login; the result's URL is opened in a browser and the
	// operation (result: Account) finishes when the authorization server redirects.
	Login(ctx context.Context, req apiv0.LoginRequest) (*apiv0.LoginStart, error)
	// Logout revokes and forgets the account's session.
	Logout(ctx context.Context, account string) error
	// Accounts lists logged-in accounts.
	Accounts(ctx context.Context) ([]apiv0.Account, error)
	// Publish writes a name record (and the head of any cluster dataset following the
	// name here), then optionally waits for the named seeds (W4).
	Publish(ctx context.Context, req apiv0.PublishRequest) (*apiv0.PublishResult, error)
	// Unpublish deletes a name record.
	Unpublish(ctx context.Context, name, account string) (*apiv0.Publication, error)
	// ResolveName resolves <handle-or-did>/<name> to its verified target.
	ResolveName(ctx context.Context, target string, req apiv0.ResolveRequest) (*apiv0.NameResolution, error)
	// Names lists an account's names (the logged-in one by default).
	Names(ctx context.Context, account string) (*apiv0.NameList, error)
	// NodeRegister writes this node's node record into the account.
	NodeRegister(ctx context.Context, req apiv0.NodeRegisterRequest) (*apiv0.NodeRecord, error)
	// Devices lists an account's node records.
	Devices(ctx context.Context, account string) (*apiv0.DeviceList, error)
	// Follow creates a mirror dataset following a name; Follows lists follows;
	// Unfollow removes one (its pins only); FollowRefresh re-resolves now.
	Follow(ctx context.Context, req apiv0.FollowRequest) (*apiv0.Follow, error)
	Follows(ctx context.Context) ([]apiv0.Follow, error)
	Unfollow(ctx context.Context, dataset string) (*apiv0.Follow, error)
	FollowRefresh(ctx context.Context, req apiv0.FollowRefreshRequest) ([]apiv0.Follow, error)
}

var _ NamesNode = (*Local)(nil)

// AuthDir is the session directory under the repo.
const AuthDir = "auth"

// namesState is the node's name-plane state.
type namesState struct {
	mu       sync.Mutex
	resolver *names.Resolver
	sessions *atproto.Sessions
	// follows is per-dataset follow bookkeeping (refresh times, errors, endings).
	follows map[string]*followState
	// jetstream is the optional hint source; polled is the last full refresh.
	jetstream *follow.Jetstream
	polled    time.Time
	// kick wakes FollowLoop after a follow/unfollow (filter update).
	kick chan struct{}
}

func (ns *namesState) wake() {
	ns.mu.Lock()
	ch := ns.kick
	ns.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Local) resolver() (*names.Resolver, error) {
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if n.names.resolver == nil || n.names.resolver.Client() != c {
		n.names.resolver = names.NewResolver(c, 0, nil)
	}
	return n.names.resolver, nil
}

func (n *Local) sessionStore() (*atproto.Sessions, error) {
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if n.names.sessions == nil {
		s, err := atproto.OpenSessions(n.Repo.Path(AuthDir))
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the session directory: %v", err)
		}
		n.names.sessions = s
	}
	return n.names.sessions, nil
}

// session returns the session for account ("" = the only one; a handle or DID).
func (n *Local) session(ctx context.Context, account string) (*atproto.Session, *atproto.Sessions, error) {
	ss, err := n.sessionStore()
	if err != nil {
		return nil, nil, err
	}
	if account == "" {
		s, err := ss.Default()
		return s, ss, err
	}
	did := account
	if !strings.HasPrefix(account, "did:") {
		all, _ := ss.List()
		found := ""
		for _, s := range all {
			if strings.EqualFold(s.Handle, strings.TrimPrefix(account, "@")) {
				found = s.DID
			}
		}
		if found == "" {
			c, err := n.atproto()
			if err != nil {
				return nil, nil, err
			}
			id, err := c.Resolve(ctx, account)
			if err != nil {
				return nil, nil, err
			}
			found = id.DID
		}
		did = found
	}
	s, err := ss.Load(did)
	if err != nil {
		return nil, nil, err
	}
	return s, ss, nil
}

func (n *Local) account(s *atproto.Session) apiv0.Account {
	return apiv0.Account{DID: s.DID, Handle: s.Handle, PDS: s.PDS, Scopes: s.Scopes, LoggedIn: s.Created, Updated: s.Updated}
}

// LoginTimeout bounds how long a login waits for the browser.
const LoginTimeout = 10 * time.Minute

func (n *Local) Login(ctx context.Context, req apiv0.LoginRequest) (*apiv0.LoginStart, error) {
	if req.Identifier == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "login needs a handle or DID")
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	ss, err := n.sessionStore()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot listen for the login callback: %v", err)
	}
	redirect := "http://" + ln.Addr().String() + atproto.CallbackPath
	flow, err := c.StartLogin(ctx, req.Identifier, redirect)
	if err != nil {
		ln.Close()
		return nil, err
	}
	got := make(chan url.Values, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != atproto.CallbackPath {
			http.NotFound(w, r)
			return
		}
		select {
		case got <- r.URL.Query():
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s: login received for %s. You can close this tab.\n", nu.Name, flow.DID)
		default:
			http.Error(w, "login already completed", http.StatusConflict)
		}
	})}
	go srv.Serve(ln)
	o := n.start("login", flow.DID, func(o *op.Operation) (any, error) {
		defer srv.Close()
		o.Stage("waiting for the browser")
		var q url.Values
		select {
		case q = <-got:
		case <-o.Context().Done():
			return nil, o.Context().Err()
		case <-time.After(LoginTimeout):
			return nil, apiv0.Errorf(apiv0.CodeTimeout, "no authorization arrived within %s", LoginTimeout).WithAction("run `" + nu.Name + " login` again and approve the request in the browser")
		}
		o.Stage("exchanging the code")
		sess, err := c.FinishLogin(o.Context(), flow, q)
		if err != nil {
			return nil, err
		}
		if err := ss.Save(sess); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot store the session: %v", err)
		}
		return n.account(sess), nil
	})
	return &apiv0.LoginStart{Operation: *o, URL: flow.AuthorizeURL, DID: flow.DID, Handle: flow.Handle, PDS: flow.PDS}, nil
}

func (n *Local) Logout(ctx context.Context, account string) error {
	s, ss, err := n.session(ctx, account)
	if err != nil {
		return err
	}
	if c, err := n.atproto(); err == nil {
		_ = c.Revoke(ctx, s) // best effort; the local copy goes regardless
	}
	return ss.Delete(s.DID)
}

func (n *Local) Accounts(ctx context.Context) ([]apiv0.Account, error) {
	ss, err := n.sessionStore()
	if err != nil {
		return nil, err
	}
	all, err := ss.List()
	if err != nil {
		return nil, err
	}
	out := []apiv0.Account{}
	for _, s := range all {
		out = append(out, n.account(s))
	}
	return out, nil
}

// saver persists session changes (nonces, refreshed tokens).
func (n *Local) saver(ss *atproto.Sessions) func(*atproto.Session) error {
	return func(s *atproto.Session) error { return ss.Save(s) }
}

// Publish writes the record first; then, if a dataset of this node follows the name,
// its head is written immediately (no poll wait) so seeds start acquiring; then the
// optional seed wait runs. Publication and wait are separate results (W4).
func (n *Local) Publish(ctx context.Context, req apiv0.PublishRequest) (*apiv0.PublishResult, error) {
	if err := names.ValidName(req.Name); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	target, err := cid.Parse(req.Target)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad target %q: %v", req.Target, err).WithAction("publish a CID, as printed by `" + nu.Name + " add`")
	}
	kind, size := req.Kind, int64(0)
	if kind == "" {
		if kind, size, err = n.headKind(target); err != nil {
			return nil, err
		}
	}
	rec := &names.Name{Target: target.String(), Kind: kind, Size: size, Note: req.Note, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if _, err := rec.Validate(); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	s, ss, err := n.session(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	w, err := c.PutRecord(ctx, s, names.CollectionName, req.Name, rec.Record(), n.saver(ss))
	if err != nil {
		return nil, err
	}
	// Fetch the publisher's own proof once (docs/follow.md "Publishing"); if the PDS lags,
	// the write's commit still binds the head.
	proof := &names.Resolution{DID: s.DID, Handle: s.Handle, Name: req.Name, Target: target, Kind: kind, Size: size, Note: req.Note, Rev: w.Rev, Resolved: time.Now(), Source: "pds"}
	if c, err := cid.Parse(w.Commit); err == nil {
		proof.Commit = c
	}
	if r, err := n.resolver(); err == nil {
		r.Forget(s.DID, req.Name)
		if fresh, err := r.Resolve(ctx, s.DID, req.Name, names.Options{Fresh: true}); err == nil && fresh.Target == target && fresh.Rev >= w.Rev {
			proof = fresh
			for _, b := range fresh.Proof {
				_ = n.Store.Put(b.CID, b.Data)
			}
		}
	}
	pub := apiv0.Publication{URI: w.URI, DID: s.DID, Handle: s.Handle, Name: req.Name, Target: target.String(), Kind: kind, Size: size, Record: w.CID, Rev: w.Rev}
	res := &apiv0.PublishResult{Publication: pub, Note: "published: the name now points at the target; this is not replication readiness (a seed holds it only when a wait or `follow ls` says so)"}

	// The publisher's own follows of the name: write the head now, bound to this proof.
	uri := "at://" + s.DID + "/" + names.CollectionName + "/" + req.Name
	var seedScope, seedDS string
	for _, scope := range []string{ScopeCluster, ScopeLocal} {
		cl, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		for ds, d := range cl.View().Datasets {
			if d.Deleted || d.Value == nil || d.Value.Source.Name != uri {
				continue
			}
			n.applyResolution(ctx, scope, cl, ds, d.Value, proof, nil)
			if seedDS == "" || scope == ScopeCluster {
				seedScope, seedDS = scope, ds
			}
		}
	}
	if len(req.WaitSeed) == 0 {
		res.Publication.Dataset = seedDS
		return res, nil
	}
	// A seed wait needs a cluster dataset following the name; create the seed layout
	// (mirror preset, every blob on every storage member) when none exists.
	if seedScope != ScopeCluster {
		ds := followDatasetName(req.Name, s.Handle)
		if _, err := n.Follow(ctx, apiv0.FollowRequest{Target: s.DID + "/" + req.Name, Dataset: ds, Scope: ScopeCluster, All: true}); err != nil {
			return res, apiv0.Errorf(apiv0.CodeFailed, "the record was published, but no cluster dataset follows %s and one could not be created: %v", req.Name, err).
				WithAction("seeds are storage members of your cluster (`"+nu.Name+" cluster init|invite`); then `"+nu.Name+" follow --cluster "+s.DID+"/"+req.Name+"`").
				WithDetail("result", res)
		}
		seedScope, seedDS = ScopeCluster, ds
	}
	res.Publication.Dataset = seedDS
	wait, err := n.DatasetWait(ctx, seedScope, seedDS, apiv0.DatasetWaitRequest{Target: target.String(), Condition: apiv0.WaitHeld, Holders: req.WaitSeed, Timeout: req.Timeout})
	res.Wait = wait
	if err != nil {
		var ae *apiv0.Error
		if errors.As(err, &ae) {
			return res, ae.WithDetail("result", res)
		}
		return res, err
	}
	return res, nil
}

// followDatasetName derives a dataset name for a follow: <handle-label>-<name>, within
// the dataset grammar.
func followDatasetName(name, handle string) string {
	base := strings.ToLower(name)
	if handle != "" {
		base = strings.ToLower(strings.SplitN(handle, ".", 2)[0]) + "-" + base
	}
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "follow"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func (n *Local) Unpublish(ctx context.Context, name, account string) (*apiv0.Publication, error) {
	if err := names.ValidName(name); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	s, ss, err := n.session(ctx, account)
	if err != nil {
		return nil, err
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	w, err := c.DeleteRecord(ctx, s, names.CollectionName, name, n.saver(ss))
	if err != nil {
		return nil, err
	}
	if r, err := n.resolver(); err == nil {
		r.Forget(s.DID, name)
	}
	return &apiv0.Publication{URI: "at://" + s.DID + "/" + names.CollectionName + "/" + name, DID: s.DID, Handle: s.Handle, Name: name, Rev: w.Rev}, nil
}

func (n *Local) nameResolution(r *names.Resolution) *apiv0.NameResolution {
	out := &apiv0.NameResolution{URI: r.URI(), DID: r.DID, Handle: r.Handle, Name: r.Name, Target: r.Target.String(), Kind: r.Kind, Size: r.Size,
		Note: r.Note, UpdatedAt: r.UpdatedAt, Rev: r.Rev, Commit: r.Commit.String(), Record: r.RecordCID.String(), Source: r.Source,
		Age: r.Age(time.Now()).Round(time.Second).String(), IdentityAge: r.IdentityAge.Round(time.Second).String()}
	out.Held = n.fullyHeld(context.Background(), r.Target.String(), map[string]bool{})
	return out
}

func parseResolveOptions(req apiv0.ResolveRequest) (names.Options, error) {
	o := names.Options{Fresh: req.Fresh, Offline: req.Offline}
	if req.MaxAge != "" {
		d, err := time.ParseDuration(req.MaxAge)
		if err != nil || d < 0 {
			return o, apiv0.Errorf(apiv0.CodeInvalidArgument, "max_age must be a duration such as 1h")
		}
		o.MaxAge = d
	}
	return o, nil
}

// resolveNameTarget resolves a name target to its root, keeping the proof blocks held.
func (n *Local) resolveNameTarget(ctx context.Context, pt target.Target, o names.Options) (*names.Resolution, error) {
	r, err := n.resolver()
	if err != nil {
		return nil, err
	}
	res, err := r.Resolve(ctx, pt.Authority, pt.Name, o)
	if err != nil {
		var del *names.ErrDeleted
		if errors.As(err, &del) {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "%v", del)
		}
		return nil, err
	}
	for _, b := range res.Proof {
		_ = n.Store.Put(b.CID, b.Data)
	}
	return res, nil
}

func (n *Local) ResolveName(ctx context.Context, t string, req apiv0.ResolveRequest) (*apiv0.NameResolution, error) {
	pt, err := target.Parse(t)
	if err != nil {
		return nil, err
	}
	if pt.Kind != target.KindName {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a public name", t).WithAction("resolve <handle-or-did>/<name>")
	}
	o, err := parseResolveOptions(req)
	if err != nil {
		return nil, err
	}
	res, err := n.resolveNameTarget(ctx, pt, o)
	if err != nil {
		return nil, err
	}
	return n.nameResolution(res), nil
}

// gatewayName is the gateway's /nu/ lookup: the resolved target (cache within TTL), or
// with held the held root of a local follow of the name.
func (n *Local) gatewayName(ctx context.Context, authority, name string, held bool) (string, string, error) {
	pt := target.Target{Kind: target.KindName, Authority: authority, Name: name}
	if held {
		res, err := n.resolveNameTarget(ctx, pt, names.Options{Offline: true})
		did := authority
		if err == nil {
			did = res.DID
		} else if !strings.HasPrefix(authority, "did:") {
			return "", "", apiv0.Errorf(apiv0.CodeUnavailable, "%s/%s has not been resolved here yet, so no held view can be selected", authority, name)
		}
		for _, f := range n.followList(ctx) {
			if f.DID == did && f.Name == name && f.Held != nil {
				return f.Held.Target, "held", nil
			}
		}
		return "", "", apiv0.Errorf(apiv0.CodeNotFound, "no follow of %s/%s holds a complete view here", authority, name)
	}
	res, err := n.resolveNameTarget(ctx, pt, names.Options{})
	if err != nil {
		return "", "", err
	}
	return res.Target.String(), res.Source, nil
}

// listNames lists and verifies an account's name records.
func (n *Local) listNames(ctx context.Context, id *atproto.Identity) ([]apiv0.NameResolution, error) {
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	r, err := n.resolver()
	if err != nil {
		return nil, err
	}
	listed, err := c.ListRecords(ctx, id, names.CollectionName, 0)
	if err != nil {
		return nil, err
	}
	out := []apiv0.NameResolution{}
	for _, l := range listed {
		name := l.URI[strings.LastIndex(l.URI, "/")+1:]
		res, err := r.Resolve(ctx, id.DID, name, names.Options{})
		if err != nil {
			continue // listing is a hint; only proven records are listed
		}
		out = append(out, *n.nameResolution(res))
	}
	slices.SortFunc(out, func(a, b apiv0.NameResolution) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// identityFor resolves account ("" = the logged-in account) to an identity.
func (n *Local) identityFor(ctx context.Context, account string) (*atproto.Identity, error) {
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	if account == "" {
		s, _, err := n.session(ctx, "")
		if err != nil {
			return nil, err
		}
		account = s.DID
	}
	return c.Resolve(ctx, account)
}

func (n *Local) Names(ctx context.Context, account string) (*apiv0.NameList, error) {
	id, err := n.identityFor(ctx, account)
	if err != nil {
		return nil, err
	}
	list, err := n.listNames(ctx, id)
	if err != nil {
		return nil, err
	}
	return &apiv0.NameList{DID: id.DID, Handle: id.Handle, Names: list}, nil
}

// nodeRKey is the record key of this node's record: its PeerId in base32 CIDv1
// libp2p-key form.
func nodeRKey(id wire.PeerID) string { return id.String() }

func (n *Local) NodeRegister(ctx context.Context, req apiv0.NodeRegisterRequest) (*apiv0.NodeRecord, error) {
	s, ss, err := n.session(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	roles := []string{names.RoleDevice}
	if req.Seed {
		roles = append(roles, names.RoleSeed)
	}
	label := req.Label
	if label == "" {
		label, _ = os.Hostname()
		if len(label) > names.MaxLabel {
			label = label[:names.MaxLabel]
		}
	}
	rec := &names.Node{Label: label, Roles: roles, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := rec.Validate(); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	rkey := nodeRKey(n.PeerID())
	w, err := c.PutRecord(ctx, s, names.CollectionNode, rkey, rec.Record(), n.saver(ss))
	if err != nil {
		return nil, err
	}
	return &apiv0.NodeRecord{URI: w.URI, DID: s.DID, Peer: n.PeerID().Base36(), Label: rec.Label, Roles: rec.Roles, CreatedAt: rec.CreatedAt, Rev: w.Rev, Self: true}, nil
}

func (n *Local) Devices(ctx context.Context, account string) (*apiv0.DeviceList, error) {
	id, err := n.identityFor(ctx, account)
	if err != nil {
		return nil, err
	}
	c, err := n.atproto()
	if err != nil {
		return nil, err
	}
	listed, err := c.ListRecords(ctx, id, names.CollectionNode, 0)
	if err != nil {
		return nil, err
	}
	out := &apiv0.DeviceList{DID: id.DID, Handle: id.Handle, Devices: []apiv0.NodeRecord{}}
	self := nodeRKey(n.PeerID())
	for _, l := range listed {
		rkey := l.URI[strings.LastIndex(l.URI, "/")+1:]
		rec, err := c.GetRecord(ctx, atproto.URI{Authority: id.DID, Collection: names.CollectionNode, RKey: rkey})
		if err != nil {
			continue
		}
		nd, err := names.DecodeNode(rec.Value)
		if err != nil || nd.Validate() != nil {
			continue
		}
		peer := rkey
		if pid, err := wire.ParsePeerID(rkey); err == nil {
			peer = pid.Base36()
		}
		out.Devices = append(out.Devices, apiv0.NodeRecord{URI: l.URI, DID: id.DID, Peer: peer, Label: nd.Label, Roles: nd.Roles, Relays: nd.Relays,
			DNSAddr: nd.DNSAddr, CreatedAt: nd.CreatedAt, Rev: rec.Rev, Self: rkey == self})
	}
	slices.SortFunc(out.Devices, func(a, b apiv0.NodeRecord) int { return strings.Compare(a.Peer, b.Peer) })
	return out, nil
}
