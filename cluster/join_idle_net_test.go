package cluster_test

import (
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/nutest"
)

// While a secret invite is live a private node accepts anyone's connection, but closes it
// when no `join` arrives within JoinIdleTimeout (0069).
func TestJoinIdleTimeout(t *testing.T) {
	old := cluster.JoinIdleTimeout
	cluster.JoinIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cluster.JoinIdleTimeout = old })
	n := nutest.New(t, 2)
	a, stranger := n.Nodes[0], n.Nodes[1]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "lab", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{Role: cluster.RoleClient}); err != nil {
		t.Fatal(err)
	}
	n.Peer(stranger, a) // admitted for the join exchange only
	eventually(t, "connection accepted", func() bool { return a.ConnectedTo(stranger.ID) })
	connected := time.Now()
	eventually(t, "idle non-member disconnected", func() bool { return !a.ConnectedTo(stranger.ID) })
	if d := time.Since(connected); d < 200*time.Millisecond {
		t.Fatalf("disconnected after %v, before the idle timeout", d)
	}
}
