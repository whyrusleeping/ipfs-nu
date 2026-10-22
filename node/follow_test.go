package node

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/follow"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// The Jetstream hint source (docs/follow.md "Update notification" 2) subscribes only to
// the followed DIDs and io.ipfs.nu.name; an event for a followed name triggers a verified
// re-resolution (the head moves to the new target through the PDS proof, not from the
// event), events for other names are ignored, and the subscription filter follows the
// follow set.
func TestFollowJetstreamHint(t *testing.T) {
	f := atfix.New(t)
	n := newLocal(t)
	n.Atproto = atproto.New(f.Config())
	ctx := context.Background()

	var mu sync.Mutex
	var subs []url.Values
	conns := make(chan *websocket.Conn, 4)
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		mu.Lock()
		subs = append(subs, ws.Request().URL.Query())
		mu.Unlock()
		conns <- ws
		<-ws.Request().Context().Done()
	}))
	defer srv.Close()
	js := &follow.Jetstream{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/subscribe", Backoff: 50 * time.Millisecond}
	n.SetJetstream(js)
	old := FollowPoll
	FollowPoll = time.Hour
	defer func() { FollowPoll = old }()
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go n.FollowLoop(lctx)

	blob := "bafkr4ic7bdlu4f7hhzjpkvzltxz5ztqfr4bhpo5f6djbl4fgc6kcjgzg7a"
	f.Pub.Put("io.ipfs.nu.name", "x", map[string]any{"$type": "io.ipfs.nu.name", "target": blob, "kind": "blob", "updatedAt": "2026-01-01T00:00:00Z"})
	fl, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/x", Dataset: "x"})
	if err != nil || fl.Observed.Target != blob {
		t.Fatalf("follow: %+v %v", fl, err)
	}
	var ws *websocket.Conn
	select {
	case ws = <-conns:
	case <-time.After(10 * time.Second):
		t.Fatal("no subscription")
	}
	mu.Lock()
	q := subs[len(subs)-1]
	mu.Unlock()
	if q.Get("wantedCollections") != "io.ipfs.nu.name" || q.Get("wantedDids") != atfix.PubDID {
		t.Fatalf("filter: %v", q)
	}
	// The publisher moves the name; the event only names it, the proof supplies the target.
	data := []byte("new bytes")
	c, _ := add(t, n, apiv0.AddRequest{Path: writeTemp(t, data), Raw: true})
	f.Pub.Put("io.ipfs.nu.name", "x", map[string]any{"$type": "io.ipfs.nu.name", "target": c.CID, "kind": "blob", "updatedAt": "2026-01-02T00:00:00Z"})
	send := func(did, rkey string) {
		ev, _ := json.Marshal(map[string]any{"did": did, "kind": "commit", "commit": map[string]any{"rev": "3zzz", "operation": "update", "collection": "io.ipfs.nu.name", "rkey": rkey}})
		if err := websocket.Message.Send(ws, string(ev)); err != nil {
			t.Fatal(err)
		}
	}
	send(atfix.PubDID, "other") // not followed: ignored
	send(atfix.PubDID, "x")
	deadline := time.Now().Add(10 * time.Second)
	for {
		fs, _ := n.Follows(ctx)
		if len(fs) == 1 && fs[0].Held != nil && fs[0].Held.Target == c.CID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow did not move on the hint: %+v", fs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f.Requests["com.atproto.sync.getRecord"] < 2 {
		t.Fatalf("the hint must be verified through the PDS: %d getRecord requests", f.Requests["com.atproto.sync.getRecord"])
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
