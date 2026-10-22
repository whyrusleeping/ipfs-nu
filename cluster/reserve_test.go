package cluster

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/store"
)

// A full store still takes signed state writes from its reserve (D17 A5); content
// admission is refused.
func TestStateReserveUnderFullStore(t *testing.T) {
	clk := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := founder(t, clk)
	st := a.opts.Store
	data := make([]byte, 1<<20)
	rand.Read(data)
	if _, err := st.AddBlobHash(bytes.NewReader(data), int64(len(data)), cid.BLAKE3); err != nil {
		t.Fatal(err)
	}
	u, err := st.Usage()
	if err != nil {
		t.Fatal(err)
	}
	// Physical − usage is exactly the (capped) state reserve: content headroom 0.
	st.SetLimits(store.Limits{Physical: u * 8 / 7, StateReserve: 16 << 20})
	s, _ := st.Stats()
	if s.Headroom != 0 || !s.Pressure || s.StateReserve < 100<<10 {
		t.Fatalf("stats: %+v", s)
	}
	small := make([]byte, 4<<10)
	rand.Read(small)
	_, err = st.AddBlobHash(bytes.NewReader(small), int64(len(small)), cid.BLAKE3)
	var be *store.BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("content add into the state reserve: %v", err)
	}
	for i := range 3 {
		if _, err := a.Write(Op{Collection: NSMember, RKey: peerID(t).String(), Value: &Member{Roles: []string{RoleStorage}, State: StateUnavailable}}); err != nil {
			t.Fatalf("member record %d under a full store: %v", i, err)
		}
	}
	if len(a.View().Members) < 3 {
		t.Fatalf("members: %d", len(a.View().Members))
	}
}
