package node

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/replica"
)

// Repair, drain, audits and release (docs/cluster.md "Sync, handoff and repair";
// docs/operations.md S1–S8, E8, E9; backlog 0048).

// RepairNode is the administration part of a cluster node beyond ClusterNode: member
// records, keys, invites, release and audits. Local and the HTTP client implement it.
type RepairNode interface {
	ClusterMemberSet(ctx context.Context, req apiv0.MemberSetRequest) (*apiv0.MemberResult, error)
	ClusterKey(ctx context.Context, req apiv0.KeyRequest) (*apiv0.KeyResult, error)
	ClusterKeyRevoke(ctx context.Context, did string) (*apiv0.KeyResult, error)
	ClusterInviteRevoke(ctx context.Context, id string) (*apiv0.MetadataState, error)
	DatasetRelease(ctx context.Context, scope, name string, req apiv0.ReleaseRequest) (*apiv0.ReleaseReport, error)
	ClusterAudit(ctx context.Context, req apiv0.AuditRequest) (*apiv0.AuditReport, error)
}

var _ RepairNode = (*Local)(nil)

// ClusterSettings are a node's repair, release and audit settings (`[cluster]` in
// config.toml over ClusterDefaults).
type ClusterSettings struct {
	Repair             bool
	DownAfter, UpAfter time.Duration
	ReleaseAuto        bool
	Settle, Grace      time.Duration
	AuditInterval      time.Duration
	AuditSamples       int
	// AuditFailures consecutive failed challenges count as failed for repair.
	AuditFailures int
	// UnknownAlert is evidence.unknown_alert (E8).
	UnknownAlert time.Duration
}

// ClusterDefaults are the settings' defaults (tests shorten them).
var ClusterDefaults = ClusterSettings{Repair: true, DownAfter: cluster.DefaultDownAfter, UpAfter: cluster.DefaultUpAfter,
	Settle: cluster.DefaultSettle, Grace: cluster.DefaultRemoveGrace, AuditInterval: time.Hour, AuditSamples: 16, AuditFailures: 3,
	UnknownAlert: 30 * time.Minute}

// EvidenceFresh is evidence.fresh (E4; tests shorten it).
var EvidenceFresh = cluster.DefaultFresh

// settings reads the cluster settings (once per daemon start; cached).
func (n *Local) settings() ClusterSettings {
	if s := n.clusters.settings.Load(); s != nil {
		return *s
	}
	s := ClusterDefaults
	if cfg, err := n.Repo.Config(); err == nil && cfg.Cluster != nil {
		cc := cfg.Cluster
		dur := func(v string, d *time.Duration) {
			if x, err := time.ParseDuration(v); err == nil && x >= 0 {
				*d = x
			}
		}
		switch cc.Repair {
		case "off", "false", "no":
			s.Repair = false
		case "auto":
			s.Repair = true
		}
		dur(cc.DownAfter, &s.DownAfter)
		dur(cc.UpAfter, &s.UpAfter)
		dur(cc.ReleaseSettle, &s.Settle)
		dur(cc.RemoveGrace, &s.Grace)
		dur(cc.AuditInterval, &s.AuditInterval)
		s.ReleaseAuto = s.ReleaseAuto || cc.ReleaseAuto
		if cc.AuditSamples > 0 {
			s.AuditSamples = cc.AuditSamples
		}
	}
	n.clusters.settings.Store(&s)
	return s
}

func (n *Local) releaseSettings() replica.ReleaseSettings {
	s := n.settings()
	return replica.ReleaseSettings{Auto: s.ReleaseAuto, Settle: s.Settle, Grace: s.Grace, Fresh: EvidenceFresh}
}

func (n *Local) repairConfig() *cluster.RepairConfig {
	s := n.settings()
	if !s.Repair {
		return nil
	}
	return &cluster.RepairConfig{DownAfter: s.DownAfter, UpAfter: s.UpAfter, Failed: n.auditFailed}
}

// remoteEvidence returns other members' reports for a dataset.
func (n *Local) remoteEvidence(name string) map[wire.PeerID]cluster.Evidence {
	if svc := n.clusters.svcPtr.Load(); svc != nil {
		return svc.Evidence(name)
	}
	return nil
}

// Audits.

type auditBook struct {
	mu    sync.Mutex
	peers map[wire.PeerID]*apiv0.AuditPeer
}

func (n *Local) noteAudit(ch apiv0.Challenge, id wire.PeerID, at time.Time) {
	b := &n.clusters.audits
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peers == nil {
		b.peers = map[wire.PeerID]*apiv0.AuditPeer{}
	}
	p := b.peers[id]
	if p == nil {
		p = &apiv0.AuditPeer{Peer: id.Base36()}
		b.peers[id] = p
	}
	p.Last = &at
	if ch.OK {
		p.Passed++
		p.Consecutive = 0
		p.LastError = ""
	} else {
		p.Failed++
		p.Consecutive++
		p.LastError = ch.Error
	}
}

func (n *Local) auditFailed(id wire.PeerID) bool {
	b := &n.clusters.audits
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[id]
	return p != nil && p.Consecutive >= max(n.settings().AuditFailures, 1)
}

func (n *Local) auditPeers() []apiv0.AuditPeer {
	b := &n.clusters.audits
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []apiv0.AuditPeer
	for _, p := range b.peers {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b apiv0.AuditPeer) int { return cmpStr(a.Peer, b.Peer) })
	return out
}

// challenge audits one member: a random chunk group of a random blob it owns in a
// placed dataset whose head structure this node holds. ok is false when there is
// nothing to challenge it on.
func (n *Local) challenge(ctx context.Context, c *cluster.Cluster, peer wire.PeerID) (apiv0.Challenge, bool) {
	m := n.replicaFor(c)
	n.clusters.mu.Lock()
	sw := n.clusters.sw
	n.clusters.mu.Unlock()
	if m == nil || sw == nil {
		return apiv0.Challenge{}, false
	}
	v := c.View()
	var names []string
	for name, d := range v.Datasets {
		if !d.Deleted && replica.Placed(d.Value) {
			names = append(names, name)
		}
	}
	rand.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	for _, name := range names {
		h, ok := v.Head(name)
		if !ok {
			continue
		}
		target, err := cid.Parse(h.Value.Target)
		if err != nil {
			continue
		}
		blobs := m.OwnedBlobs(name, target, func(pl *cluster.Placement, mh cid.Multihash) bool { return pl.Owns(peer, mh) })
		if len(blobs) == 0 {
			continue
		}
		b := blobs[rand.IntN(len(blobs))]
		g := uint64(0)
		if gs := blob.Groups(b.Size); gs > 1 {
			g = rand.Uint64N(gs)
		}
		ch := apiv0.Challenge{Peer: peer.Base36(), Dataset: name, CID: b.CID.String(), Group: g}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		_, err = proto.Challenge(cctx, proto.HostOpener(sw.Host(), peer), b.CID.Multihash(), g)
		cancel()
		ch.MS = time.Since(start).Milliseconds()
		ch.OK = err == nil
		if err != nil {
			ch.Error = err.Error()
		}
		n.noteAudit(ch, peer, c.Now())
		return ch, true
	}
	return apiv0.Challenge{}, false
}

// ClusterAudit challenges storage members and scrubs local content now. Audits are not
// completion reports (E9): they never raise confirmations.
func (n *Local) ClusterAudit(ctx context.Context, req apiv0.AuditRequest) (*apiv0.AuditReport, error) {
	c, err := n.netCluster()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, noCluster()
	}
	rep := &apiv0.AuditReport{Challenges: []apiv0.Challenge{},
		Note: "audits show a member served sampled verified bytes in time; they are not completion reports and never count as confirmed copies (E9)"}
	var peers []wire.PeerID
	if req.Peer != "" {
		id, err := wire.ParsePeerID(req.Peer)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad PeerId %q: %v", req.Peer, err)
		}
		peers = []wire.PeerID{id}
	} else {
		peers = n.storagePeers(c)
	}
	if len(peers) > 0 && n.clusters.svcPtr.Load() == nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "auditing members needs networking").WithAction("run it against the daemon (`" + nu.Name + " daemon`)")
	}
	for _, id := range peers {
		for range max(req.Samples, 1) {
			if ch, ok := n.challenge(ctx, c, id); ok {
				rep.Challenges = append(rep.Challenges, ch)
			}
		}
	}
	if req.Scrub > 0 || req.Full {
		if m := n.replicaFor(c); m != nil {
			sample := req.Scrub
			if req.Full {
				sample = 0
			}
			s := m.Scrub(ctx, sample)
			rep.Scrub = &s
		}
	}
	return rep, nil
}

func (n *Local) storagePeers(c *cluster.Cluster) []wire.PeerID {
	var out []wire.PeerID
	v := c.View()
	for id, m := range v.Members {
		if id != n.PeerID() && !m.Deleted && m.Value.State != cluster.StateRemoved && slices.Contains(m.Value.Roles, cluster.RoleStorage) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// auditLoop runs periodic audits: a sampled local scrub and one challenge per connected
// storage member every audit interval.
func (n *Local) auditLoop(ctx context.Context, c *cluster.Cluster) {
	s := n.settings()
	if s.AuditInterval <= 0 {
		return
	}
	t := time.NewTicker(s.AuditInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		svc := n.clusters.svcPtr.Load()
		for _, id := range n.storagePeers(c) {
			if svc != nil && svc.Connected(id) {
				n.challenge(ctx, c, id)
			}
		}
		if m := n.replicaFor(c); m != nil {
			m.Scrub(ctx, s.AuditSamples)
		}
	}
}

// Members, keys and invites.

func (n *Local) adminCluster() (*cluster.Cluster, error) {
	c, err := n.netCluster()
	if err == nil && c == nil {
		err = noCluster()
	}
	return c, err
}

func (n *Local) memberInfo(c *cluster.Cluster, id wire.PeerID) apiv0.MemberInfo {
	m := c.View().Members[id]
	mi := apiv0.MemberInfo{Peer: id.Base36(), Self: id == n.PeerID()}
	if m.Value != nil {
		r := m.Value
		mi.Roles, mi.State, mi.Tags, mi.Addrs, mi.Auto, mi.Capacity, mi.StoreLimit, mi.Label = r.Roles, r.State, r.Tags, r.Addrs, r.Auto, r.Capacity, r.StoreLimit, r.Label
	}
	return mi
}

// ClusterMemberSet edits a member record (`member set`, `member drain`).
func (n *Local) ClusterMemberSet(ctx context.Context, req apiv0.MemberSetRequest) (*apiv0.MemberResult, error) {
	c, err := n.adminCluster()
	if err != nil {
		return nil, err
	}
	id, err := wire.ParsePeerID(req.Peer)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad PeerId %q: %v", req.Peer, err)
	}
	m, ok := c.View().Members[id]
	if !ok || m.Deleted || m.Value == nil || m.Value.State == cluster.StateRemoved {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not a member", id.Base36())
	}
	rec := *m.Value
	out := &apiv0.MemberResult{}
	switch req.State {
	case "":
	case cluster.StateActive, cluster.StateDraining, cluster.StateUnavailable:
		rec.State, rec.Auto = req.State, false
	case cluster.StateRemoved:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "use `"+nu.Name+" cluster member rm` to remove a member")
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown state %q", req.State).WithAction("use active, draining or unavailable")
	}
	if req.Capacity > 0 {
		rec.Capacity = req.Capacity
	}
	if req.StoreLimit > 0 {
		rec.StoreLimit = req.StoreLimit
	}
	if req.Tags != nil {
		rec.Tags = req.Tags
	}
	if req.Label != "" {
		rec.Label = req.Label
	}
	if req.State == cluster.StateDraining {
		out.Warnings = append(out.Warnings, "draining: the member owns nothing and keeps serving; it releases its copies as new owners confirm them; `"+nu.Name+" cluster member rm --dry-run` shows what is still unconfirmed")
	}
	if req.State == cluster.StateUnavailable {
		out.Warnings = append(out.Warnings, "set by an admin: a repair authority does not lift it; set the member active again when it is back")
	}
	w, err := c.Write(cluster.Op{Collection: cluster.NSMember, RKey: id.String(), Value: &rec})
	if err != nil {
		return nil, writeErr(err)
	}
	out.Metadata = n.ack(ctx, c, w)
	out.Member = n.memberInfo(c, id)
	return out, nil
}

// ClusterMemberRemove removes a member (S6): refused, unless forced, while the member is
// an active storage member of a cluster holding content or any content lacks k confirmed
// owner copies without it; DryRun only reports.
func (n *Local) ClusterMemberRemove(ctx context.Context, req apiv0.ClusterMemberRemoveRequest) (*apiv0.MemberRemoval, error) {
	c, err := n.adminCluster()
	if err != nil {
		return nil, err
	}
	id, err := wire.ParsePeerID(req.Peer)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad PeerId %q: %v", req.Peer, err)
	}
	v := c.View()
	m, ok := v.Members[id]
	if !ok || m.Deleted || m.Value == nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not a member", id.Base36())
	}
	rep := &apiv0.MemberRemoval{Peer: id.Base36(), State: m.Value.State, DryRun: req.DryRun, Unconfirmed: []apiv0.Unconfirmed{}}
	if slices.Contains(m.Value.Roles, cluster.RoleStorage) && m.Value.State != cluster.StateRemoved {
		rep.Unconfirmed = n.unconfirmed(c)
		if m.Value.State == cluster.StateActive && n.holdsContent(c) {
			rep.Reasons = append(rep.Reasons, "the member is an active owner: drain it first (`"+nu.Name+" cluster member drain "+id.Base36()+"`) so new owners acquire and confirm its blobs")
		}
	}
	for _, u := range rep.Unconfirmed {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("%s: %d %s(s) below k confirmed owner copies", u.Dataset, u.Units, u.Unit))
	}
	rep.Safe = len(rep.Reasons) == 0
	if req.DryRun {
		return rep, nil
	}
	if !rep.Safe && !req.Force {
		return rep, apiv0.Errorf(apiv0.CodeFailed, "removing %s now could leave content without k confirmed copies: %s", id.Base36(), strings.Join(rep.Reasons, "; ")).
			WithDetail("result", rep).
			WithAction("wait until `" + nu.Name + " cluster member rm --dry-run " + id.Base36() + "` reports it safe, or pass --force to remove it anyway (destructive: the listed content may lose copies)")
	}
	rec := *m.Value
	rec.State, rec.Auto = cluster.StateRemoved, false
	w, err := c.Write(cluster.Op{Collection: cluster.NSMember, RKey: id.String(), Value: &rec})
	if err != nil {
		return nil, writeErr(err)
	}
	md := n.ack(ctx, c, w)
	rep.Metadata, rep.Removed, rep.Forced, rep.State = &md, true, !rep.Safe, cluster.StateRemoved
	return rep, nil
}

// holdsContent reports whether any dataset has a head.
func (n *Local) holdsContent(c *cluster.Cluster) bool {
	v := c.View()
	for name, d := range v.Datasets {
		if _, ok := v.Head(name); ok && !d.Deleted {
			return true
		}
	}
	return false
}

// unconfirmed lists, per dataset with a head, the units with fewer than k confirmed
// owner copies in the current epoch (S2 counting, members only, this node's report
// included), with the owners missing.
func (n *Local) unconfirmed(c *cluster.Cluster) []apiv0.Unconfirmed {
	v := c.View()
	out := []apiv0.Unconfirmed{}
	var names []string
	for name, d := range v.Datasets {
		if !d.Deleted {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		d, _ := v.Dataset(name)
		h, ok := v.Head(name)
		if !ok {
			continue
		}
		target, err := cid.Parse(h.Value.Target)
		if err != nil {
			continue
		}
		if !replica.Placed(d) {
			if d.Collection == nil || (d.Policy.Replicas == 0 && !d.Policy.All) {
				continue // no guaranteed copies
			}
			if cm := n.clusters.collPtr.Load(); cm != nil {
				if root, ok := cm.MergedRoot(name); ok {
					target = root
				}
			}
			cs := n.evalContent(c, name, target).cs
			if cs.State != cluster.ReadyReady {
				u := apiv0.Unconfirmed{Dataset: name, Target: target.String(), Unit: "item", Missing: cs.Missing, Detail: cs.State}
				if cs.Copies != nil {
					u.Units = cs.Copies.Below
				}
				out = append(out, u)
			}
			continue
		}
		m := n.replicaFor(c)
		if m == nil {
			continue
		}
		roots := []cid.CID{target}
		var live string
		if d.Policy.History != nil {
			hs := histFor(c, name, d)
			live, roots = hs.ret.ID, append(roots, hs.liveRoots()...)
		}
		groups, pl, known := m.Groups(name, roots...)
		u := apiv0.Unconfirmed{Dataset: name, Target: target.String(), Unit: "blob", Missing: []apiv0.MissingOwner{}}
		if !known {
			u.Detail = "this node does not hold the structure, so it cannot count copies; run it on a storage member"
			out = append(out, u)
			continue
		}
		ev := map[wire.PeerID]cluster.Evidence{}
		if s, ok := m.Status(name); ok {
			ev[n.PeerID()] = cluster.Evidence{Report: s.Reported, Received: c.Now()}
		}
		for id, e := range n.remoteEvidence(name) {
			if id != n.PeerID() {
				ev[id] = e
			}
		}
		in := cluster.ReleaseInput{Target: target.String(), Live: live, Policy: d.PolicyHash(), Epoch: pl.Epoch, K: pl.K(), Self: n.PeerID(),
			Evidence: ev, Now: c.Now(), Fresh: EvidenceFresh, Members: v.IsMember}
		for _, g := range groups {
			conf, miss := in.Confirm(g.Owners, true)
			if len(conf) >= in.K && pl.Feasible().OK {
				continue
			}
			u.Units += g.Units
			for _, mo := range miss {
				e := apiv0.MissingOwner{Peer: peerStr(mo.Peer), Reason: mo.Reason}
				if !slices.Contains(u.Missing, e) {
					u.Missing = append(u.Missing, e)
				}
			}
		}
		if u.Units > 0 {
			out = append(out, u)
		}
	}
	return out
}

// ClusterKey creates a key on this node (no DID) or grants a role to a DID.
func (n *Local) ClusterKey(ctx context.Context, req apiv0.KeyRequest) (*apiv0.KeyResult, error) {
	c, err := n.adminCluster()
	if err != nil {
		return nil, err
	}
	if req.DID == "" {
		did, err := c.NewKey()
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot create a key: %v", err)
		}
		return &apiv0.KeyResult{DID: did, Held: true}, nil
	}
	if !strings.HasPrefix(req.DID, "did:key:") {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a did:key", req.DID)
	}
	v := c.View()
	if req.DID == v.Genesis.RootKey {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the root key's authority is fixed by genesis")
	}
	ops := []cluster.Op{}
	switch req.Role {
	case cluster.RoleAdmin:
		if req.Dataset != "" {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "an admin key writes every dataset; --dataset applies to writers")
		}
	case cluster.RoleWriter:
		if req.Dataset == "" {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a writer key needs --dataset")
		}
		d, ok := v.Datasets[req.Dataset]
		if !ok || d.Deleted || d.Value == nil {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q", req.Dataset)
		}
		rec := *d.Value
		if !slices.Contains(rec.Writers, req.DID) {
			rec.Writers = append(slices.Clone(rec.Writers), req.DID)
		}
		ops = append(ops, cluster.Op{Collection: cluster.NSDataset, RKey: req.Dataset, Value: &rec})
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown role %q", req.Role).WithAction("use admin or writer")
	}
	ops = append([]cluster.Op{{Collection: cluster.NSKey, RKey: req.DID, Value: &cluster.Key{Role: req.Role}}}, ops...)
	w, err := c.Write(ops...)
	if err != nil {
		return nil, writeErr(err)
	}
	md := n.ack(ctx, c, w)
	return &apiv0.KeyResult{DID: req.DID, Role: req.Role, Held: slices.Contains(c.HeldKeys(), req.DID), Metadata: &md}, nil
}

// ClusterKeyRevoke revokes a key at its last commit this node holds (a key that never
// wrote is simply deleted).
func (n *Local) ClusterKeyRevoke(ctx context.Context, did string) (*apiv0.KeyResult, error) {
	c, err := n.adminCluster()
	if err != nil {
		return nil, err
	}
	v := c.View()
	if did == v.Genesis.RootKey {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "the root key cannot be revoked; see re-genesis in docs/operations.md B8")
	}
	k := v.Keys[did]
	if k == nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not an authorized key", did)
	}
	op := cluster.Op{Collection: cluster.NSKey, RKey: did, Delete: true}
	if h, ok := c.Held(did); ok && h.Commit.Defined() {
		op = cluster.Op{Collection: cluster.NSKey, RKey: did, Value: &cluster.Key{Role: k.Role, Revoked: &cluster.Revocation{After: h.Commit, Rev: h.Rev}}}
	}
	w, err := c.Write(op)
	if err != nil {
		return nil, writeErr(err)
	}
	md := n.ack(ctx, c, w)
	return &apiv0.KeyResult{DID: did, Role: k.Role, Revoked: true, Metadata: &md}, nil
}

// ClusterInviteRevoke tombstones a secret invite.
func (n *Local) ClusterInviteRevoke(ctx context.Context, id string) (*apiv0.MetadataState, error) {
	c, err := n.adminCluster()
	if err != nil {
		return nil, err
	}
	if in, ok := c.View().Invites[id]; !ok || in.Deleted {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no outstanding invite %q", id)
	}
	w, err := c.Write(cluster.Op{Collection: cluster.NSInvite, RKey: id, Delete: true})
	if err != nil {
		return nil, writeErr(err)
	}
	md := n.ack(ctx, c, w)
	return &md, nil
}

// DatasetRelease reports (DryRun) or applies safe release on this member (S1–S4, S8).
func (n *Local) DatasetRelease(ctx context.Context, scope, name string, req apiv0.ReleaseRequest) (*apiv0.ReleaseReport, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	d, ok := c.View().Dataset(name)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope)
	}
	if !replica.Placed(d) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not placed by blobs (collections release items with `"+nu.Name+" cluster collection`)", name)
	}
	m := n.replicaFor(c)
	if m == nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "placement state unavailable")
	}
	rep, ok := m.ReleaseReport(name, req.DryRun)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "this member keeps nothing of %q (no head, or not placed here)", name)
	}
	return &rep, nil
}

// Status: repair authority, alerts.

// repairInfo is the repair-authority state of c.
func (n *Local) repairInfo(svc *cluster.Service) *apiv0.RepairInfo {
	ri := &apiv0.RepairInfo{Authorities: []string{}, DownAfterMS: n.settings().DownAfter.Milliseconds()}
	if svc == nil {
		ri.Detail = "not running: start the daemon"
		return ri
	}
	self, peers := svc.RepairAuthorities()
	ri.Enabled = self
	if self {
		ri.Authorities = append(ri.Authorities, n.PeerID().Base36())
	}
	for _, p := range peers {
		ri.Authorities = append(ri.Authorities, p.Base36())
	}
	ri.Online = len(ri.Authorities) > 0
	if !ri.Online {
		ri.Detail = "no online repair authority: failed members are not marked unavailable and their blobs are not re-placed"
	}
	return ri
}

func noAuthorityAlert() apiv0.Alert {
	return apiv0.Alert{Code: apiv0.AlertNoRepairAuthority, Detail: "automatic repair is unavailable: no admin daemon with repair on is online, so lost copies are not restored",
		Action: "start a daemon holding an admin key (`" + nu.Name + " config set cluster.repair auto`), or mark a failed member yourself: `" + nu.Name + " cluster member set <peer> --state unavailable`"}
}

// clusterAlerts are the cluster-wide alert conditions of info (members, repair, audits,
// scrub).
func (n *Local) clusterAlerts(c *cluster.Cluster, svc *cluster.Service, info *apiv0.ClusterInfo) []apiv0.Alert {
	out := []apiv0.Alert{}
	if c.Mode() == cluster.ModeLocal {
		return out
	}
	s := n.settings()
	for _, mi := range info.Members {
		switch {
		case mi.State == cluster.StateUnavailable:
			how := "set by an admin"
			if mi.Auto {
				how = "marked by a repair authority"
			}
			out = append(out, apiv0.Alert{Code: apiv0.AlertMemberUnavailable, Peer: mi.Peer, Detail: "member is unavailable (" + how + "); its blobs are re-placed on other members",
				Action: "bring it back (it becomes active after cluster.up_after of heartbeats when marked automatically), or drain and remove it"})
		case mi.State == cluster.StateActive && !mi.Self && slices.Contains(mi.Roles, cluster.RoleStorage) && svc != nil:
			id, err := wire.ParsePeerID(mi.Peer)
			if err != nil {
				continue
			}
			last := svc.LastHeard(id)
			if (last.IsZero() && !mi.Connected) || (!last.IsZero() && c.Now().Sub(last) > 3*ClusterInterval+EvidenceFresh) {
				out = append(out, apiv0.Alert{Code: apiv0.AlertMemberSilent, Peer: mi.Peer, Detail: "no heartbeat from this storage member recently; its reports go stale (unknown, not loss)",
					Action: "check the member and its network; a repair authority marks it unavailable after cluster.down_after (" + s.DownAfter.String() + ")"})
			}
		}
	}
	if info.Repair != nil && !info.Repair.Online && n.holdsContent(c) {
		out = append(out, noAuthorityAlert())
	}
	for _, a := range info.Audits {
		if a.Consecutive > 0 {
			out = append(out, apiv0.Alert{Code: apiv0.AlertAuditFailed, Peer: a.Peer, Detail: fmt.Sprintf("%d consecutive failed audit challenge(s): %s", a.Consecutive, a.LastError),
				Action: "check the member's disk and network; repeated failures mark it unavailable when a repair authority runs"})
		}
	}
	if sc := info.Scrub; sc != nil && len(sc.Corrupt) > 0 {
		out = append(out, apiv0.Alert{Code: apiv0.AlertScrubFailed, Detail: fmt.Sprintf("the last scrub found %d corrupt object(s) here; they were dropped and are fetched again from other members", len(sc.Corrupt)),
			Action: "check this machine's disk (SMART, filesystem errors)"})
	}
	return out
}

// contentAlerts adds the repair state and alerts of a readiness result (E8).
func (n *Local) contentAlerts(c *cluster.Cluster, name string, cs *apiv0.ContentState) {
	if c.Mode() == cluster.ModeLocal {
		return
	}
	svc := n.clusters.svcPtr.Load()
	online := false
	if svc != nil {
		self, peers := svc.RepairAuthorities()
		online = self || len(peers) > 0
	}
	key := c.ID.String() + "/" + name
	now := c.Now()
	n.reps.mu.Lock()
	if n.reps.unknown == nil {
		n.reps.unknown = map[string]time.Time{}
	}
	if cs.State == cluster.ReadyUnknown {
		if _, ok := n.reps.unknown[key]; !ok {
			n.reps.unknown[key] = now
		}
	} else {
		delete(n.reps.unknown, key)
	}
	since, unknown := n.reps.unknown[key]
	n.reps.mu.Unlock()
	switch cs.State {
	case cluster.ReadyDegraded:
		if online {
			cs.Repair = "in_progress"
		} else {
			cs.Repair = "unavailable"
		}
		cs.Alerts = append(cs.Alerts, apiv0.Alert{Code: apiv0.AlertDegraded, Dataset: name,
			Detail: "previously ready content has fewer than k confirmed copies; see copies and missing", Action: "repair restores copies from surviving members when an authority is online and capacity allows"})
	case cluster.ReadyUnknown:
		if unknown && now.Sub(since) > n.settings().UnknownAlert {
			cs.Alerts = append(cs.Alerts, apiv0.Alert{Code: apiv0.AlertUnknownTooLong, Dataset: name,
				Detail: "readiness has been unknown (stale evidence) for " + now.Sub(since).Round(time.Second).String(), Action: "check the members listed as stale in missing"})
		}
	}
	if (cs.State == cluster.ReadyDegraded || cs.State == cluster.ReadyUnknown) && !online {
		if cs.Repair == "" {
			cs.Repair = "unavailable"
		}
		a := noAuthorityAlert()
		a.Dataset = name
		cs.Alerts = append(cs.Alerts, a)
	}
}
