package fetch_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/nutest/timeouts"
)

// stalled is a provider source that never answers until its context ends (a DHT whose
// bootstrap is stuck). late offers a peer only after a delay.
type stalled struct{ cancelled chan struct{} }

func (s stalled) Find(ctx context.Context, _ cid.CID, _ func(fetch.Peer)) error {
	<-ctx.Done()
	close(s.cancelled)
	return ctx.Err()
}

type late struct {
	after time.Duration
	peer  fetch.Peer
}

func (l late) Find(ctx context.Context, _ cid.CID, found func(fetch.Peer)) error {
	select {
	case <-time.After(l.after):
		found(l.peer)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A hinted get starts transferring while a provider source is stalled (0076): the fetch
// completes promptly from the hinted peer and the stalled source is cancelled. A peer
// that a source names only later is still used when the known ones cannot serve.
func TestFetchStartsBeforeSlowSourcesFinish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	src := openStore(t, t.TempDir())
	data := randBytes(7, 256<<10)
	blobID, err := src.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	b := newHost(t, nil)
	holder := connect(t, b, newHost(t, src))

	stuck := stalled{cancelled: make(chan struct{})}
	dst := openStore(t, t.TempDir())
	f := fetch.New(fetch.Config{Store: dst, Providers: fetch.Multi{stuck}})
	started := time.Now()
	if _, err := f.Fetch(ctx, blobID, fetch.Options{Peers: []fetch.Peer{holder}}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > time.Second {
		t.Fatalf("hinted fetch waited %s for a stalled source", d)
	}
	select {
	case <-stuck.cancelled:
	default:
		t.Fatal("the stalled source was not cancelled when the fetch finished")
	}
	if !bytes.Equal(readBlob(t, dst, blobID), data) {
		t.Fatal("blob differs")
	}

	// No hint and no immediate candidate: the transfer waits for the first one.
	dst2 := openStore(t, t.TempDir())
	f = fetch.New(fetch.Config{Store: dst2, Providers: fetch.Multi{stalled{cancelled: make(chan struct{})}, late{after: 50 * time.Millisecond, peer: holder}}})
	if _, err := f.Fetch(ctx, blobID, fetch.Options{}); err != nil {
		t.Fatal(err)
	}

	// A hinted peer that does not hold the content: the late source's peer serves it.
	empty := connect(t, b, newHost(t, openStore(t, t.TempDir())))
	dst3 := openStore(t, t.TempDir())
	f = fetch.New(fetch.Config{Store: dst3, Providers: late{after: 100 * time.Millisecond, peer: holder}})
	if _, err := f.Fetch(ctx, blobID, fetch.Options{Peers: []fetch.Peer{empty}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBlob(t, dst3, blobID), data) {
		t.Fatal("blob differs")
	}
}
