package fetch

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/store"
)

// committer group-commits the whole raw blobs a run fetches. Each blob is staged and
// synced on its own goroutine (the fsyncs overlap); one store.Batch Commit at a time then
// renames everything queued meanwhile into place and inserts it in one transaction, so
// the store's single database connection does one commit per group rather than per blob.
// A blob counts as stored only once its commit returns, as with Store.Put.
type committer struct {
	mu      sync.Mutex
	queue   []staged
	running bool
}

type staged struct {
	b    *store.Batch
	done chan error
}

// put stores a verified whole object: raw blobs through the group commit, others with
// Store.Put.
func (r *run) put(c cid.CID, data []byte) error {
	if c.Codec() != cid.Raw {
		return r.st.Put(c, data)
	}
	b := r.st.Batch()
	got, err := b.AddBlobHash(bytes.NewReader(data), int64(len(data)), c.Hash())
	if err == nil && got.Multihash() != c.Multihash() {
		err = fmt.Errorf("fetch: staged %s as %s", c, got)
	}
	if err == nil {
		err = b.Sync()
	}
	if err != nil {
		b.Abort()
		return err
	}
	return r.commits.submit(b)
}

func (cm *committer) submit(b *store.Batch) error {
	done := make(chan error, 1)
	cm.mu.Lock()
	cm.queue = append(cm.queue, staged{b, done})
	if !cm.running {
		cm.running = true
		go cm.loop()
	}
	cm.mu.Unlock()
	return <-done
}

// loop commits the queue in groups until it is empty.
func (cm *committer) loop() {
	for {
		cm.mu.Lock()
		q := cm.queue
		cm.queue = nil
		if len(q) == 0 {
			cm.running = false
			cm.mu.Unlock()
			return
		}
		cm.mu.Unlock()
		all := q[0].b
		for _, s := range q[1:] {
			all.Append(s.b)
		}
		err := all.Commit()
		for _, s := range q {
			s.done <- err
		}
	}
}
