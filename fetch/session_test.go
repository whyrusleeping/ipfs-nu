package fetch_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
)

// A hinted peer dials on demand, then reconnects during the same fetch after its
// connection is dropped. With one worker, every request before the drop has been
// verified; none of those ranges may be requested on the replacement connection.
func TestSessionPeerReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src := openStore(t, t.TempDir())
	data := randBytes(6, 2<<20)
	id, err := src.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	provider, client := newHost(t, src), newHost(t, nil)
	sessions, err := host.NewSessions(client, host.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.Close() })
	hints, err := lookup.ParseHints("nu://"+id.String()+"?peer="+provider.ID().String()+"&addr="+provider.Addrs()[0].String(), lookup.DefaultHintLimits())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var asked []blob.Range
	var peers fetch.Static
	if err := (fetch.Hinted{Sessions: sessions, Hints: hints}).Find(ctx, id, func(p fetch.Peer) {
		if p.ID() != provider.ID() {
			t.Fatalf("hinted peer %s, want %s", p.ID(), provider.ID())
		}
		peers = append(peers, recordPeer{Peer: p, mu: &mu, ranges: &asked})
	}); err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || len(client.Connections()) != 0 {
		t.Fatal("hint discovery must offer one peer without dialing")
	}

	dst := openStore(t, t.TempDir())
	f := fetch.New(fetch.Config{Store: dst, Providers: peers, PeerRequests: 1, ChunkGroups: 16})
	var old *host.Conn
	var cut int
	res, err := f.Get(ctx, id, fetch.GetOptions{Options: fetch.Options{
		Size: uint64(len(data)),
		Progress: func(p fetch.Progress) {
			if old != nil || p.Bytes < 256<<10 {
				return
			}
			old, _ = client.Connection(provider.ID())
			mu.Lock()
			cut = len(asked)
			mu.Unlock()
			client.Disconnect(provider.ID())
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	current, ok := client.Connection(provider.ID())
	if old == nil || old.Context().Err() == nil || !ok || current == old {
		t.Fatal("fetch did not replace the dropped connection")
	}
	if cut == 0 || len(asked) <= cut {
		t.Fatalf("no requests before/after disconnect: cut %d, requests %v", cut, asked)
	}
	for _, a := range asked[cut:] {
		for _, held := range asked[:cut] {
			if a.Start < held.End && held.Start < a.End {
				t.Fatalf("re-requested %v, already verified %v", a, held)
			}
		}
	}
	if len(res.Dropped) != 0 || res.Bytes != int64(len(data)) || !bytes.Equal(readBlob(t, dst, id), data) {
		t.Fatalf("reconnected fetch differs: %+v", res)
	}
}
