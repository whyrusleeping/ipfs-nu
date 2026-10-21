package kad

import (
	"context"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestResolveQUICBootstrapDNSAndLiteral(t *testing.T) {
	key, _ := wire.GenerateNodeKey()
	id := key.Public().PeerID()
	for _, input := range []string{"/ip4/127.0.0.1/udp/4001/quic-v1/p2p/" + id.String(), "/dns4/localhost/udp/4001/quic-v1/p2p/" + id.String()} {
		a, err := wire.ParseMultiaddr(input, 1024)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := resolveQUICAddress(context.Background(), id, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) == 0 {
			t.Fatal("no result")
		}
		for _, got := range resolved {
			if !IsQUICAddress(got, id) || strings.HasPrefix(got.String(), "/dns") {
				t.Fatalf("not literal QUIC peer address: %s", got)
			}
		}
	}
}
