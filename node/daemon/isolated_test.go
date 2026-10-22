package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// A member of an isolated cluster refuses at startup config naming non-member endpoints
// (0069); member peers stay allowed.
func TestIsolatedRefusesExternalEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "vault", Mode: "isolated"}); err != nil {
		t.Fatal(err)
	}
	self := n.NetCluster().View().MemberIDs()[0]
	n.Close()
	k, _ := wire.GenerateNodeKey()
	stranger := k.Public().PeerID()
	set := func(kv map[string]string) {
		for k, v := range kv {
			if err := r.ConfigSet(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	set(map[string]string{
		"swarm.peers":      `["/ip4/10.0.0.2/udp/4171/quic-v1/p2p/` + self.String() + `", "/ip4/10.0.0.3/udp/4171/quic-v1/p2p/` + stranger.String() + `"]`,
		"swarm.relays":     `["/ip4/1.2.3.4/udp/4171/quic-v1/p2p/` + stranger.String() + `"]`,
		"follow.jetstream": "wss://jetstream.example/subscribe",
	})
	opts := Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", NoNetwork: true, Logger: slog.New(slog.DiscardHandler)}
	_, err = Start(ctx, opts)
	if !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("isolated start with external endpoints: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"swarm.relays", stranger.String(), "follow.jetstream"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal does not name %s: %s", want, msg)
		}
	}
	if strings.Contains(msg, "10.0.0.2") {
		t.Fatalf("member peer refused: %s", msg)
	}
	set(map[string]string{"swarm.peers": `["/ip4/10.0.0.2/udp/4171/quic-v1/p2p/` + self.String() + `"]`, "swarm.relays": "[]", "follow.jetstream": `""`})
	d, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("isolated start with member peers only: %v", err)
	}
	d.Close()
}
