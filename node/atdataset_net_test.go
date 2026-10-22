package node_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/nutest/atfix"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// An atrepo dataset on a networked cluster (0054): the admin fetches from the PDS and
// writes the head with its rev. The storage member fetches both revisions only from
// the admin, the second as an exact structure/record diff, and readiness reaches two
// copies including selected blobs. Account status still propagates.
func TestAtrepoClusterDataset(t *testing.T) {
	f := atfix.New(t)
	old := node.ClusterInterval
	node.ClusterInterval = 200 * time.Millisecond
	defer func() { node.ClusterInterval = old }()
	nw := nutest.New(t, 2)
	a, b := nw.Nodes[0], nw.Nodes[1]
	traffic := &atrepoTraffic{byRoot: map[cid.CID]atrepoTransfer{}}
	srv := a.Swarm.Server()
	if err := a.Swarm.Host().Handle(proto.TreeID, srv.HandlerLimit(), func(s *host.Stream) {
		cap := &atrepoCapture{ServerStream: s}
		srv.Serve(proto.TreeID, s.Conn().PeerID(), cap)
		traffic.record(cap)
	}); err != nil {
		t.Fatal(err)
	}
	a.Local.Atproto = atproto.New(f.Config())
	b.Local.Atproto = atproto.New(f.Config())
	ctx, cancel := context.WithTimeout(context.Background(), 4*nutest.Timeout)
	defer cancel()
	if _, err := a.Local.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Local.ClusterInvite(ctx, apiv0.ClusterInviteRequest{Peer: b.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Local.ClusterJoin(ctx, apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "membership", func() bool {
		return len(a.Local.NetCluster().View().MemberIDs()) == 2 && len(b.Local.NetCluster().View().MemberIDs()) == 2
	})
	pub := f.Pub
	for i := range 128 {
		pub.Put("app.bsky.feed.post", fmt.Sprintf("r%03d", i), map[string]any{"$type": "app.bsky.feed.post", "text": fmt.Sprintf("post %d", i)})
	}
	blob := pub.PutBlob([]byte("archived picture"))
	pub.Put("app.bsky.feed.post", "3aaa", map[string]any{"$type": "app.bsky.feed.post", "image": atfix.BlobRef(blob, "image/png", 16), "video": atfix.BlobRef(pub.Oversized, "video/mp4", cid.MaxWholeObject+1)})

	d, err := a.Local.DatasetCreate(ctx, node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "pub", Archive: true, SourceAt: "at://" + atfix.PubDID, Blobs: true, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if d.Head == nil || d.Head.Rev != pub.Rev() || len(d.Warnings) != 1 { // no zone spread
		t.Fatalf("create: %+v %v", d.Head, d.Warnings)
	}
	md, err := a.Local.DatasetWait(ctx, node.ScopeCluster, "pub", apiv0.DatasetWaitRequest{Target: d.Head.Target, Condition: apiv0.WaitMetadata, Acks: 2})
	if err != nil || !md.Satisfied {
		t.Fatalf("metadata: %+v %v", md, err)
	}
	var bd *apiv0.Dataset
	eventually(t, "member sees the head", func() bool {
		bd, err = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
		return err == nil && bd.Head != nil && bd.Head.Rev == pub.Rev() && bd.Atrepo != nil && bd.Atrepo.Status == "active"
	})
	waitReady := func(target string) {
		t.Helper()
		eventually(t, "member complete and two copies ready", func() bool {
			bd, err = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
			return err == nil && bd.Head.Target == target && bd.Content.Local != nil && bd.Content.Local.Complete && bd.Content.State == cluster.ReadyReady
		})
	}
	waitReady(d.Head.Target)
	oldRoot, _ := cid.Parse(d.Head.Target)
	oldBlocks := atrepoBlocks(t, b.Local.Store, oldRoot)
	full := traffic.get(oldRoot)
	if info, ok, err := b.Local.Store.Has(blob); err != nil || !ok || !info.Complete {
		t.Fatalf("member blob: %+v %v %v", info, ok, err)
	}
	if full.frames != len(oldBlocks) || full.req.Profile != proto.ProfileAtrepo {
		t.Fatalf("first transfer: %+v, blocks %d", full, len(oldBlocks))
	}
	var repos int
	f.Set(func(f *atfix.Fixture) { repos = f.Requests["com.atproto.sync.getRepo"] })
	if repos != 1 {
		t.Fatalf("getRepo requests: %d (only the admin fetches)", repos)
	}

	// A since-diff update and a deactivation reach the member through the signed state.
	pub.Put("app.bsky.feed.post", "3bbb", map[string]any{"$type": "app.bsky.feed.post", "text": "second", "createdAt": "2026-01-01T00:00:00Z"})
	if d, err = a.Local.DatasetUpdate(ctx, node.ScopeCluster, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Mode != "since" {
		t.Fatalf("update: %+v %v", d.Atrepo, err)
	}
	waitReady(d.Head.Target)
	newRoot, _ := cid.Parse(d.Head.Target)
	newBlocks := atrepoBlocks(t, b.Local.Store, newRoot)
	var wantFrames, wantBytes int
	for c, size := range newBlocks {
		if _, held := oldBlocks[c]; !held {
			wantFrames++
			wantBytes += size
		}
	}
	diff := traffic.get(newRoot)
	if diff.frames != wantFrames || diff.bytes != wantBytes || diff.bytes >= full.bytes/4 || diff.markers == 0 || len(diff.req.Have) != 1 || diff.req.Have[0] != oldRoot {
		t.Fatalf("second transfer %+v; want %d frames/%d bytes; first %+v", diff, wantFrames, wantBytes, full)
	}
	t.Logf("member replication: first %d frames/%d bytes; second %d frames/%d bytes + %d have markers", full.frames, full.bytes, diff.frames, diff.bytes, diff.markers)
	f.Set(func(f *atfix.Fixture) { repos = f.Requests["com.atproto.sync.getRepo"]; f.Pub.Status = "takendown" })
	if repos != 2 {
		t.Fatalf("PDS exports %d, want only two admin fetches", repos)
	}
	if d, err = a.Local.DatasetUpdate(ctx, node.ScopeCluster, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Head.Status != cluster.AccountTakendown {
		t.Fatalf("takedown: %+v %v", d.Head, err)
	}
	eventually(t, "member sees the status", func() bool {
		bd, err = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
		return err == nil && bd.Head.Rev == pub.Rev() && bd.Head.Status == cluster.AccountTakendown && bd.Atrepo.Redistribution == node.RedistPrivate
	})
	vs, err := a.Local.DatasetVersions(ctx, node.ScopeCluster, "pub")
	if err != nil || len(vs.Versions) != 2 || vs.Versions[1].Rev != pub.Rev() {
		t.Fatalf("versions: %+v %v", vs, err)
	}
}

// Capture actual negotiated tree streams, without replacing the production fetch path.
type atrepoCapture struct {
	proto.ServerStream
	request, response bytes.Buffer
}

func (s *atrepoCapture) Read(b []byte) (int, error) {
	n, err := s.ServerStream.Read(b)
	s.request.Write(b[:n])
	return n, err
}
func (s *atrepoCapture) Write(b []byte) (int, error) {
	n, err := s.ServerStream.Write(b)
	s.response.Write(b[:n])
	return n, err
}

type atrepoTransfer struct {
	req                    proto.TreeRequest
	frames, bytes, markers int
}
type atrepoTraffic struct {
	mu     sync.Mutex
	byRoot map[cid.CID]atrepoTransfer
}

func (s *atrepoTraffic) record(cap *atrepoCapture) {
	b, err := wire.ReadMessage(&cap.request, proto.MaxRequestSize)
	if err != nil {
		return
	}
	req, err := proto.ParseTreeRequest(b)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.byRoot[req.Root]
	got.req = req
	if _, err := wire.ReadMessage(&cap.response, proto.MaxBytesHeaderSize); err != nil {
		return
	}
	for {
		b, err := wire.ReadMessage(&cap.response, proto.MaxTreeFrame)
		if err == io.EOF {
			break
		}
		if err != nil || len(b) == 0 {
			return
		}
		if b[0] == 1 {
			got.frames++
			got.bytes += len(b) - 1
		}
		if b[0] == 2 {
			got.markers++
		}
	}
	s.byRoot[req.Root] = got
}
func (s *atrepoTraffic) get(c cid.CID) atrepoTransfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byRoot[c]
}
func atrepoBlocks(t *testing.T, st *store.Store, root cid.CID) map[cid.CID]int {
	t.Helper()
	out := map[cid.CID]int{}
	var walk func(cid.CID)
	walk = func(c cid.CID) {
		if _, ok := out[c]; ok {
			return
		}
		b, err := st.Get(c)
		if err != nil {
			t.Fatal(err)
		}
		out[c] = len(b)
		ls, err := atrepo.Links(c, b)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			walk(l)
		}
	}
	walk(root)
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(nutest.Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
