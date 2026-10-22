package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Dataset scopes: `nu dataset …` acts on the implicit local cluster, `nu cluster dataset
// …` on the networked one (docs/cluster.md "Local cluster").
const (
	ScopeLocal   = "local"
	ScopeCluster = "cluster"
)

// ClusterNode is the cluster part of a node; Local and the HTTP client implement it.
type ClusterNode interface {
	ClusterInit(ctx context.Context, req apiv0.ClusterInitRequest) (*apiv0.ClusterInfo, error)
	ClusterInvite(ctx context.Context, req apiv0.ClusterInviteRequest) (*apiv0.ClusterInvite, error)
	ClusterJoin(ctx context.Context, req apiv0.ClusterJoinRequest) (*apiv0.ClusterInfo, error)
	ClusterStatus(ctx context.Context) (*apiv0.ClusterStatus, error)
	ClusterMemberRemove(ctx context.Context, req apiv0.ClusterMemberRemoveRequest) (*apiv0.MemberRemoval, error)
	DatasetCreate(ctx context.Context, scope string, req apiv0.DatasetCreateRequest) (*apiv0.Dataset, error)
	Datasets(ctx context.Context, scope string) ([]apiv0.Dataset, error)
	Dataset(ctx context.Context, scope, name string) (*apiv0.Dataset, error)
	DatasetUpdate(ctx context.Context, scope, name string, req apiv0.DatasetUpdateRequest) (*apiv0.Dataset, error)
	DatasetWait(ctx context.Context, scope, name string, req apiv0.DatasetWaitRequest) (*apiv0.WaitResult, error)
}

var _ ClusterNode = (*Local)(nil)

// ClusterNet is what the cluster service needs from the running swarm.
type ClusterNet interface {
	Host() *host.Host
	Connect(ctx context.Context, id wire.PeerID, addrs []wire.Multiaddr) error
	Addrs() []wire.Multiaddr
}

// ClusterInterval is the heartbeat/anti-entropy interval of cluster services this node
// starts (tests shorten it).
var ClusterInterval = cluster.DefaultInterval

// clusterState is the node's local and networked cluster (zero value: none opened).
type clusterState struct {
	mu    sync.Mutex
	local *cluster.Cluster
	net   *cluster.Cluster
	svc   *cluster.Service
	sw    ClusterNet
	coll  *collection.Manager
	// svcPtr and collPtr mirror svc and coll for readers that must not take mu.
	svcPtr  atomic.Pointer[cluster.Service]
	collPtr atomic.Pointer[collection.Manager]
	// gated mirrors net for the connection gate, which must not take mu (the host
	// rechecks connections while cluster operations hold it).
	gated atomic.Pointer[cluster.Cluster]
	// Repair, release and audit settings and state (repair.go).
	settings    atomic.Pointer[ClusterSettings]
	audits      auditBook
	auditCancel context.CancelFunc
}

func (cs *clusterState) setNet(c *cluster.Cluster) {
	cs.net = c
	cs.gated.Store(c)
}

func (n *Local) clusterDir(kind string) string { return n.Repo.Path("cluster/" + kind) }

func (n *Local) clusterOpts(kind string) cluster.Options {
	return cluster.Options{Dir: n.clusterDir(kind), Store: n.Store}
}

// netCluster returns the networked cluster, opening it on first use; nil if none.
func (n *Local) netCluster() (*cluster.Cluster, error) {
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return n.netClusterLocked()
}

func (n *Local) netClusterLocked() (*cluster.Cluster, error) {
	cs := &n.clusters
	if cs.net == nil && cluster.Exists(n.clusterDir("net")) {
		c, err := cluster.Open(n.clusterOpts("net"))
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the cluster state in %s: %v", n.clusterDir("net"), err)
		}
		cs.setNet(c)
	}
	return cs.net, nil
}

// localCluster returns the implicit local cluster, creating it on first use.
func (n *Local) localCluster() (*cluster.Cluster, error) {
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.local != nil {
		return cs.local, nil
	}
	var c *cluster.Cluster
	var err error
	if cluster.Exists(n.clusterDir("local")) {
		c, err = cluster.Open(n.clusterOpts("local"))
	} else {
		c, err = cluster.Create(n.clusterOpts("local"), "local", cluster.ModeLocal, n.PeerID(), nil)
	}
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the local cluster in %s: %v", n.clusterDir("local"), err)
	}
	cs.local = c
	if cs.sw != nil {
		n.startReplica(c)
	}
	return c, nil
}

// GateSwarm composes the networked cluster's connection policy with allow (which may be
// nil). Install the result as the swarm's AllowPeer so private and isolated clusters gate
// from the moment the host listens; it follows cluster changes (init, join, removals).
func (n *Local) GateSwarm(allow host.ConnectionGater) host.ConnectionGater {
	_, _ = n.netCluster()
	self := n.PeerID()
	return func(id wire.PeerID) bool {
		if allow != nil && !allow(id) {
			return false
		}
		c := n.clusters.gated.Load()
		if c == nil {
			return true
		}
		g := cluster.Gate(c, self)
		return g == nil || g(id)
	}
}

// PrivateNetwork reports whether the node belongs to a private or isolated cluster, whose
// swarm must not run LAN discovery or other public mechanisms.
func (n *Local) PrivateNetwork() bool {
	c, _ := n.netCluster()
	return c != nil && (c.Mode() == cluster.ModePrivate || c.Mode() == cluster.ModeIsolated)
}

// AttachNetwork plugs a running swarm in: it becomes the Fetcher/Net users of the node
// see, and the networked cluster's service (if any) starts on it.
func (n *Local) AttachNetwork(sw ClusterNet) error {
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.sw = sw
	if cs.local != nil || cluster.Exists(n.clusterDir("local")) {
		if cs.local == nil {
			if c, err := cluster.Open(n.clusterOpts("local")); err == nil {
				cs.local = c
			}
		}
		if cs.local != nil {
			n.startReplica(cs.local)
		}
	}
	c, err := n.netClusterLocked()
	if err != nil || c == nil {
		return err
	}
	return n.startServiceLocked(c)
}

func (n *Local) startServiceLocked(c *cluster.Cluster) error {
	cs := &n.clusters
	if cs.svc != nil || cs.sw == nil {
		return nil
	}
	if cs.coll != nil { // opened without networking (a query before attach): reopen with it
		_ = cs.coll.Close()
		cs.coll = nil
		cs.collPtr.Store(nil)
	}
	m, err := n.collectionsLocked(c)
	if err != nil {
		return err
	}
	svc, err := cluster.StartService(cluster.ServiceConfig{Cluster: c, Host: cs.sw.Host(), Key: n.Key, Connect: cs.sw.Connect, Addrs: cs.sw.Addrs,
		Usage: n.usage, Interval: ClusterInterval, Holdings: m.Holdings, HoldingsMarks: m.HoldingsMarks, Reports: n.reportsOf(c, m), Repair: n.repairConfig})
	if err != nil {
		return err
	}
	cs.svc = svc
	cs.svcPtr.Store(svc)
	m.Start()
	n.startReplica(c)
	actx, cancel := context.WithCancel(context.Background())
	cs.auditCancel = cancel
	go n.auditLoop(actx, c)
	return nil
}

// usage reports free disk space (unknown: 0) and physical bytes the store holds.
func (n *Local) usage() (free, used int64) {
	st, _ := n.Store.Stats()
	return max(st.Free, 0), st.Bytes
}

// DetachNetwork stops the cluster service; call it before closing the swarm.
func (n *Local) DetachNetwork() { n.closeClusters() }

func (n *Local) closeClusters() {
	cs := &n.clusters
	cs.mu.Lock()
	svc, coll := cs.svc, cs.coll
	cs.svc, cs.sw, cs.coll = nil, nil, nil
	if cs.auditCancel != nil {
		cs.auditCancel()
		cs.auditCancel = nil
	}
	cs.svcPtr.Store(nil)
	cs.collPtr.Store(nil)
	cs.mu.Unlock()
	if svc != nil {
		_ = svc.Close()
	}
	if coll != nil {
		_ = coll.Close()
	}
	n.closeReplicas()
}

// ClusterService returns the running cluster service, if any (tests, diagnostics).
func (n *Local) ClusterService() *cluster.Service {
	n.clusters.mu.Lock()
	defer n.clusters.mu.Unlock()
	return n.clusters.svc
}

// NetCluster returns the networked cluster, if any (tests, diagnostics).
func (n *Local) NetCluster() *cluster.Cluster {
	c, _ := n.netCluster()
	return c
}

func noCluster() error {
	return apiv0.Errorf(apiv0.CodeNotFound, "this node is not in a networked cluster").
		WithAction("create one with `" + nu.Name + " cluster init <name>` or join one with `" + nu.Name + " cluster join <token>`")
}

func writeErr(err error) error {
	if errors.Is(err, cluster.ErrNoKey) {
		return apiv0.Errorf(apiv0.CodeForbidden, "%v", err).
			WithAction("run this on a node holding an admin key of the cluster (see `" + nu.Name + " cluster status`)")
	}
	return err
}

func (n *Local) ClusterInit(ctx context.Context, req apiv0.ClusterInitRequest) (*apiv0.ClusterInfo, error) {
	if req.Mode == "" {
		req.Mode = cluster.ModePrivate
	}
	switch req.Mode {
	case cluster.ModePublic, cluster.ModePrivate, cluster.ModeIsolated:
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown mode %q", req.Mode).WithAction("use public, private or isolated")
	}
	if req.Name == "" || len(req.Name) > 64 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a cluster name of 1 to 64 bytes is required")
	}
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c, err := n.netClusterLocked(); err != nil {
		return nil, err
	} else if c != nil {
		return nil, apiv0.Errorf(apiv0.CodeConflict, "this node already belongs to cluster %q (%s)", c.Genesis().Name, c.ID).
			WithAction("a daemon belongs to at most one networked cluster; use another " + nu.EnvPrefix + "PATH for a second one")
	}
	var addrs []string
	for _, a := range n.Addrs() {
		if len(addrs) < 16 {
			addrs = append(addrs, a.String())
		}
	}
	c, err := cluster.Create(n.clusterOpts("net"), req.Name, req.Mode, n.PeerID(), addrs)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot create the cluster: %v", err)
	}
	cs.setNet(c)
	if err := n.startServiceLocked(c); err != nil {
		return nil, err
	}
	if cs.sw != nil {
		cs.sw.Host().RecheckConnections()
	}
	info := n.infoLocked(c, cs.svc)
	info.Warnings = append(info.Warnings, "the root key is in "+n.clusterDir("net")+"/keys; copy it offline (this node also holds an admin key for daily use)")
	return info, nil
}

func (n *Local) ClusterInvite(ctx context.Context, req apiv0.ClusterInviteRequest) (*apiv0.ClusterInvite, error) {
	c, err := n.netCluster()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, noCluster()
	}
	role := req.Role
	if role == "" {
		role = cluster.RoleStorage
	}
	if role != cluster.RoleStorage && role != cluster.RoleClient {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown role %q", role).WithAction("use storage or client")
	}
	ttl := 24 * time.Hour
	if req.Expires != "" {
		if ttl, err = time.ParseDuration(req.Expires); err != nil || ttl <= 0 {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "expires must be a positive duration such as 24h")
		}
	}
	now := time.Now()
	v := c.View()
	tok := &cluster.Token{Genesis: c.Genesis(), Expires: now.Add(ttl).Truncate(time.Second)}
	for did, k := range v.Keys {
		if k.Role == cluster.RoleAdmin && k.Revoked == nil && len(tok.Keys) < 8 {
			tok.Keys = append(tok.Keys, did)
		}
	}
	slices.Sort(tok.Keys)
	tok.Boot = n.bootPeers(c)
	if len(tok.Boot) == 0 {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "no member address is known to put in the invite").
			WithAction("start the daemon (`" + nu.Name + " daemon`) so the invite can name this node's addresses")
	}
	out := &apiv0.ClusterInvite{Expires: tok.Expires}
	var w *cluster.Written
	if req.Peer != "" {
		id, err := wire.ParsePeerID(req.Peer)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad PeerId %q: %v", req.Peer, err)
		}
		tok.Peer = id
		w, err = c.Write(cluster.Op{Collection: cluster.NSMember, RKey: id.String(), Value: &cluster.Member{Roles: []string{role}, Capacity: req.Capacity, Tags: req.Tags, State: cluster.StateActive}})
		if err != nil {
			return nil, writeErr(err)
		}
		out.Peer = id.Base36()
	} else {
		tok.InviteID, tok.Secret = cluster.NewSecret()
		w, err = c.Write(cluster.Op{Collection: cluster.NSInvite, RKey: tok.InviteID, Value: &cluster.Invite{ProofKey: cluster.ProofKey(tok.Secret), Roles: []string{role},
			Capacity: req.Capacity, Tags: req.Tags, Expires: cluster.FormatTime(tok.Expires)}})
		if err != nil {
			return nil, writeErr(err)
		}
		out.InviteID = tok.InviteID
	}
	if out.Token, err = tok.Encode(); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot encode the invite: %v", err)
	}
	out.Metadata = n.ack(ctx, c, w)
	return out, nil
}

// bootPeers lists this node and other connected members with their addresses (at most
// cluster.MaxBoot).
func (n *Local) bootPeers(c *cluster.Cluster) []cluster.Boot {
	n.clusters.mu.Lock()
	svc := n.clusters.svc
	n.clusters.mu.Unlock()
	v := c.View()
	addrsOf := func(id wire.PeerID) []string {
		var out []string
		if m, ok := v.Members[id]; ok && m.Value != nil {
			out = append(out, m.Value.Addrs...)
		}
		if svc != nil {
			if p, ok := svc.Peers()[id]; ok && p.Heartbeat != nil {
				out = append(out, p.Heartbeat.Addrs...)
			}
		}
		slices.Sort(out)
		out = slices.Compact(out)
		if len(out) > cluster.MaxBootAddrs {
			out = out[:cluster.MaxBootAddrs]
		}
		return out
	}
	var boot []cluster.Boot
	self := cluster.Boot{Peer: n.PeerID()}
	for _, a := range n.Addrs() {
		if len(self.Addrs) < cluster.MaxBootAddrs {
			self.Addrs = append(self.Addrs, a.String())
		}
	}
	if len(self.Addrs) > 0 {
		boot = append(boot, self)
	}
	for _, id := range v.MemberIDs() {
		if id == n.PeerID() || len(boot) >= cluster.MaxBoot || svc == nil || !svc.Connected(id) {
			continue
		}
		if a := addrsOf(id); len(a) > 0 {
			boot = append(boot, cluster.Boot{Peer: id, Addrs: a})
		}
	}
	return boot
}

func (n *Local) ClusterJoin(ctx context.Context, req apiv0.ClusterJoinRequest) (*apiv0.ClusterInfo, error) {
	tok, err := cluster.ParseToken(req.Token)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).WithAction("paste the whole nuc1… token from `" + nu.Name + " cluster invite`")
	}
	if time.Now().After(tok.Expires) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the invite expired at %s", tok.Expires.Format(time.RFC3339)).
			WithAction("ask an admin for a fresh invite")
	}
	if tok.Peer != "" && tok.Peer != n.PeerID() {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the invite is for peer %s, not this node (%s)", tok.Peer.Base36(), n.PeerID().Base36()).
			WithAction("ask for an invite with `--peer " + n.PeerID().Base36() + "`, or a secret invite")
	}
	cs := &n.clusters
	cs.mu.Lock()
	if c, err := n.netClusterLocked(); err != nil {
		cs.mu.Unlock()
		return nil, err
	} else if c != nil {
		cs.mu.Unlock()
		return nil, apiv0.Errorf(apiv0.CodeConflict, "this node already belongs to cluster %q (%s)", c.Genesis().Name, c.ID).
			WithAction("a daemon belongs to at most one networked cluster")
	}
	if cs.sw == nil {
		cs.mu.Unlock()
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "joining needs networking").WithAction("start `" + nu.Name + " daemon` and run the join again")
	}
	c, err := cluster.JoinToken(n.clusterOpts("net"), tok)
	if err == nil {
		cs.setNet(c)
		err = n.startServiceLocked(c)
	}
	svc := cs.svc
	cs.mu.Unlock()
	fail := func(err error) (*apiv0.ClusterInfo, error) {
		n.closeClusters()
		cs.mu.Lock()
		cs.setNet(nil)
		cs.mu.Unlock()
		_ = os.RemoveAll(n.clusterDir("net"))
		return nil, err
	}
	if err != nil {
		return fail(err)
	}
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := svc.Join(jctx, tok); err != nil {
		return fail(apiv0.Errorf(apiv0.CodeUnavailable, "no bootstrap member admitted this node: %v", err).
			WithAction("check that a member named in the invite is online and reachable over UDP, and that the invite is unused and unexpired"))
	}
	if err := svc.WaitSynced(jctx); err != nil {
		return fail(apiv0.Errorf(apiv0.CodeUnavailable, "joined, but the cluster state did not arrive: %v", err).
			WithAction("for an invite made with --peer, a bootstrap member must already know the member record; try again once it has synced"))
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.sw != nil {
		cs.sw.Host().RecheckConnections()
	}
	return n.infoLocked(c, svc), nil
}

func (n *Local) ClusterStatus(ctx context.Context) (*apiv0.ClusterStatus, error) {
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := &apiv0.ClusterStatus{}
	if cs.local == nil && cluster.Exists(n.clusterDir("local")) {
		if c, err := cluster.Open(n.clusterOpts("local")); err == nil {
			cs.local = c
		}
	}
	if cs.local != nil {
		out.Local = n.infoLocked(cs.local, nil)
	}
	c, err := n.netClusterLocked()
	if err != nil {
		return nil, err
	}
	if c != nil {
		out.Network = n.infoLocked(c, cs.svc)
	}
	return out, nil
}

// ack reports which storage members hold a write's commit, waiting briefly for one.
func (n *Local) ack(ctx context.Context, c *cluster.Cluster, w *cluster.Written) apiv0.MetadataState {
	md := apiv0.MetadataState{Signer: w.DID, Commit: w.Commit.String(), Rev: w.Rev, Required: 1, AckedBy: []string{}}
	var acked []wire.PeerID
	if c.Mode() == cluster.ModeLocal {
		acked = []wire.PeerID{n.PeerID()}
	} else if svc := n.ClusterService(); svc != nil {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		acked = svc.WaitAcked(wctx, w.DID, w.Rev, 1)
		cancel()
	} else if m, ok := c.View().Members[n.PeerID()]; ok && !m.Deleted && m.Value.State != cluster.StateRemoved && slices.Contains(m.Value.Roles, cluster.RoleStorage) {
		acked = []wire.PeerID{n.PeerID()} // offline: this storage member holds the commit
	}
	for _, id := range acked {
		md.AckedBy = append(md.AckedBy, id.Base36())
	}
	md.Acknowledged = len(acked) >= md.Required
	return md
}

func (n *Local) infoLocked(c *cluster.Cluster, svc *cluster.Service) *apiv0.ClusterInfo {
	v := c.View()
	self := n.PeerID()
	info := &apiv0.ClusterInfo{ID: c.ID.String(), Name: v.Genesis.Name, Mode: v.Genesis.Mode, Self: self.Base36(),
		Member: c.Mode() == cluster.ModeLocal || v.IsMember(self), Datasets: 0, Ignored: v.Ignored, HeldBack: v.HeldBack,
		Keys: []apiv0.ClusterKey{}, Members: []apiv0.MemberInfo{}, Repos: []apiv0.RepoInfo{}}
	held := c.HeldKeys()
	for did, k := range v.Keys {
		info.Keys = append(info.Keys, apiv0.ClusterKey{DID: did, Role: k.Role, Revoked: k.Revoked != nil, Held: slices.Contains(held, did)})
	}
	slices.SortFunc(info.Keys, func(a, b apiv0.ClusterKey) int { return cmpStr(a.Role+a.DID, b.Role+b.DID) })
	var peers map[wire.PeerID]cluster.PeerStatus
	if svc != nil {
		peers = svc.Peers()
	}
	for id, m := range v.Members {
		if m.Deleted {
			continue
		}
		mi := n.memberInfo(c, id)
		if svc != nil {
			mi.Connected = id == self || svc.Connected(id)
		}
		if p, ok := peers[id]; ok && p.Heartbeat != nil {
			t := p.Received
			mi.LastHeartbeat, mi.Free, mi.Used, mi.SkewMS = &t, p.Heartbeat.Free, p.Heartbeat.Used, p.Skew.Milliseconds()
			if p.Skew > 30*time.Second || p.Skew < -30*time.Second {
				info.Warnings = append(info.Warnings, "clock skew of "+p.Skew.String()+" with "+id.Base36())
			}
		}
		info.Members = append(info.Members, mi)
	}
	slices.SortFunc(info.Members, func(a, b apiv0.MemberInfo) int { return cmpStr(a.Peer, b.Peer) })
	for did, r := range v.Repos {
		ri := apiv0.RepoInfo{DID: did, Role: r.Role, Rev: r.Rev, Status: r.Status}
		if r.Held.Defined() {
			ri.Commit = r.Held.String()
		}
		info.Repos = append(info.Repos, ri)
	}
	slices.SortFunc(info.Repos, func(a, b apiv0.RepoInfo) int { return cmpStr(a.DID, b.DID) })
	for _, d := range v.Datasets {
		if !d.Deleted {
			info.Datasets++
		}
	}
	for id := range v.Invites {
		if _, ok := v.LiveInvite(id, c.Now()); ok {
			info.Invites++
		}
	}
	for _, b := range c.Boot() {
		info.Boot = append(info.Boot, b.Peer.Base36())
	}
	if !info.Member && c.Mode() != cluster.ModeLocal {
		info.Warnings = append(info.Warnings, "this node has no member record (yet): it is joining, or it was removed")
	}
	if c.Mode() != cluster.ModeLocal {
		info.Repair, info.Audits = n.repairInfo(svc), n.auditPeers()
		if m := n.replicaFor(c); m != nil {
			info.Scrub = m.LastScrub()
		}
		info.Alerts = n.clusterAlerts(c, svc, info)
	}
	return info
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (n *Local) scopeCluster(scope string) (*cluster.Cluster, error) {
	switch scope {
	case ScopeLocal:
		return n.localCluster()
	case ScopeCluster:
		c, err := n.netCluster()
		if err == nil && c == nil {
			err = noCluster()
		}
		return c, err
	}
	return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown dataset scope %q", scope)
}

func (n *Local) DatasetCreate(ctx context.Context, scope string, req apiv0.DatasetCreateRequest) (*apiv0.Dataset, error) {
	if !cluster.ValidName(req.Name) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dataset name %q must match [a-z0-9-]{1,64}", req.Name)
	}
	if req.Replicas < 0 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "replicas must not be negative")
	}
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	// Names are unique per daemon across its local and networked cluster.
	for _, other := range []string{ScopeLocal, ScopeCluster} {
		oc, err := n.scopeCluster(other)
		if err != nil {
			continue
		}
		if _, ok := oc.View().Dataset(req.Name); ok {
			return nil, apiv0.Errorf(apiv0.CodeConflict, "dataset %q already exists in the %s cluster", req.Name, other).
				WithAction("dataset names are unique per daemon; pick another name")
		}
	}
	coll, err := collectionPolicy(req)
	if err != nil {
		return nil, err
	}
	replicas := req.Replicas
	if replicas == 0 && coll == nil {
		replicas = 2
		if scope == ScopeLocal {
			replicas = 1
		}
	}
	src := cluster.Source{Local: true}
	if in := req.Ingest; in != nil {
		if coll == nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "ingest applies to collections").WithAction("add --collection")
		}
		if in.Timeout != "" {
			if d, err := collection.ParseDuration(in.Timeout); err != nil || d <= 0 {
				return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "ingest timeout %q must be a duration such as 2h", in.Timeout)
			}
		}
		src.Ingest = &cluster.Ingest{Hosts: in.Hosts}
		if in.MaxBytes != 0 || in.Attempts != 0 || in.Timeout != "" {
			src.Ingest.Limits = &cluster.IngestLimits{MaxBytes: in.MaxBytes, Attempts: in.Attempts, Timeout: in.Timeout}
		}
	}
	pol := cluster.Policy{Replicas: replicas, All: req.All, Where: req.Where, Spread: req.Spread}
	if pol.All {
		pol.Replicas = 0
	}
	if err := presetPolicy(req, &pol, &src, coll != nil); err != nil {
		return nil, err
	}
	if err := pol.Validate(); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	var warnings []string
	if c.Mode() != cluster.ModeLocal && !pol.All && pol.Spread == "" && pol.Replicas > 1 && coll == nil {
		// Team default (docs/cluster.md "Datasets and updates"): spread over zones only
		// when every eligible member is zone-tagged and there are enough zones.
		pl := cluster.NewPlacement(c.View(), cluster.Policy{Replicas: pol.Replicas, Where: pol.Where}, n.PeerID())
		zones, tagged := map[string]bool{}, true
		for _, m := range pl.Members {
			if z := m.Tags["zone"]; z != "" {
				zones[z] = true
			} else {
				tagged = false
			}
		}
		switch {
		case tagged && len(zones) >= int(pol.Replicas):
			pol.Spread = "zone"
		default:
			warnings = append(warnings, fmt.Sprintf("no zone spread: %d distinct zone(s) among %d eligible storage member(s), and every member must be tagged zone=…; copies may share a failure domain (set --spread to require one)", len(zones), len(pl.Members)))
		}
	}
	w, err := c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: req.Name, Value: &cluster.Dataset{Source: src,
		Policy: pol, Writers: req.Writers, Collection: coll, Note: req.Note}})
	if err != nil {
		if errors.Is(err, cluster.ErrNoKey) {
			return nil, writeErr(err)
		}
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "cannot write the dataset record: %v", err)
	}
	d := n.datasetInfo(c, req.Name)
	d.Metadata = n.ack(ctx, c, w)
	d.Warnings = warnings
	return d, nil
}

func (n *Local) Datasets(ctx context.Context, scope string) ([]apiv0.Dataset, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	v := c.View()
	out := []apiv0.Dataset{}
	for name, d := range v.Datasets {
		if !d.Deleted {
			out = append(out, *n.datasetInfo(c, name))
		}
	}
	slices.SortFunc(out, func(a, b apiv0.Dataset) int { return cmpStr(a.Name, b.Name) })
	return out, nil
}

func (n *Local) Dataset(ctx context.Context, scope, name string) (*apiv0.Dataset, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	if _, ok := c.View().Dataset(name); !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope).
			WithAction("list datasets with `" + nu.Name + " dataset ls` or `" + nu.Name + " cluster dataset ls`")
	}
	if m := n.replicaFor(c); m != nil && c.Mode() == cluster.ModeLocal {
		if _, ok := m.Status(name); !ok {
			m.Reconcile() // a daemonless command: this node's own sync state is its evidence
		}
	}
	return n.datasetInfo(c, name), nil
}

func (n *Local) datasetInfo(c *cluster.Cluster, name string) *apiv0.Dataset {
	v := c.View()
	d := v.Datasets[name]
	out := &apiv0.Dataset{Name: name, Cluster: ScopeLocal, Source: "local", Signer: d.Signer,
		Metadata: apiv0.MetadataState{AckedBy: []string{}}, Content: apiv0.ContentState{State: "none", Detail: "no head written yet"}}
	if c.Mode() != cluster.ModeLocal {
		out.Cluster = c.ID.String()
	}
	if d.Value != nil {
		out.Replicas, out.Writers, out.Note = d.Value.Policy.Replicas, d.Value.Writers, d.Value.Note
		out.All, out.Where, out.Spread = d.Value.Policy.All, d.Value.Policy.Where, d.Value.Policy.Spread
		out.History, out.SourcePath = historyPolicy(d.Value.Policy.History), d.Value.Source.Path
		if d.Value.Source.Name != "" {
			out.Source, out.SourceName = "name", d.Value.Source.Name
		}
		if cp := d.Value.Collection; cp != nil {
			out.Collection = &apiv0.DatasetCollection{Merge: cp.MergeMode(), Facets: map[string]apiv0.FacetDecl{}}
			for k, f := range cp.Facets {
				out.Collection.Facets[k] = apiv0.FacetDecl{Ptr: f.Ptr, Type: f.Type}
			}
			if cp.Expire != nil {
				out.Collection.Expire = cp.Expire.Facet + "=" + cp.Expire.After
			}
			if in := d.Value.Source.Ingest; in != nil {
				out.Collection.Ingest = &apiv0.DatasetIngest{Hosts: in.Hosts}
				if l := in.Limits; l != nil {
					out.Collection.Ingest.MaxBytes, out.Collection.Ingest.Attempts, out.Collection.Ingest.Timeout = l.MaxBytes, l.Attempts, l.Timeout
				}
			}
		}
	}
	for did, h := range v.Heads[name] {
		if h.Deleted {
			continue
		}
		out.Heads = append(out.Heads, apiv0.DatasetHead{Target: h.Value.Target, Kind: h.Value.Kind, Signer: did, HLC: h.HLC.String()})
	}
	slices.SortFunc(out.Heads, func(a, b apiv0.DatasetHead) int { return cmpStr(a.Signer, b.Signer) })
	if h, ok := v.Head(name); ok {
		out.Head = &apiv0.DatasetHead{Target: h.Value.Target, Kind: h.Value.Kind, Signer: h.Signer, HLC: h.HLC.String()}
		if d.Value == nil || d.Value.Collection == nil {
			for _, o := range out.Heads {
				if o.Target != h.Value.Target {
					out.Displaced = append(out.Displaced, o)
				}
			}
		}
		t, err := cid.Parse(h.Value.Target)
		if d.Value != nil && d.Value.Collection != nil {
			// Collection readiness binds to the merged catalog root (floor placement).
			err = apiv0.Errorf(apiv0.CodeUnavailable, "no merged catalog yet")
			if cm := n.clusters.collPtr.Load(); cm != nil {
				if root, ok := cm.MergedRoot(name); ok {
					t, err = root, nil
				}
			}
		}
		if err == nil {
			out.Content = n.evalContent(c, name, t).cs
			if d.Value != nil && d.Value.Policy.History != nil {
				hs := histFor(c, name, d.Value)
				a := n.evalTarget(c, name, t, &liveBind{id: hs.ret.ID, roots: hs.liveRoots()}).cs
				out.Archive = &a
			}
		}
	}
	return out
}
