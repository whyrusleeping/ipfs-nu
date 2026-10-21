package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// `nu relay` serves circuit v2 HOP on an ephemeral port with the repo's identity and
// stops cleanly on Ctrl-C; a wildcard listener without --advertise is a usage error.
func TestCLIRelayServesAndStops(t *testing.T) {
	r, _, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out, errb syncBuf
	if code := runCtx(context.Background(), []string{"--nu-path", r.Root(), "relay", "--listen", "/ip4/0.0.0.0/udp/0/quic-v1"}, &out, &errb); code != apiv0.ExitUsage {
		t.Fatalf("wildcard without --advertise: exit %d, stderr %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "--advertise") {
		t.Fatalf("stderr does not point at --advertise: %s", errb.String())
	}

	out, errb = syncBuf{}, syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runCtx(ctx, []string{"--nu-path", r.Root(), "--json", "relay", "--listen", "127.0.0.1:0", "--max-reservations", "4"}, &out, &errb)
	}()
	var st relayStatus
	waitFor(t, "relay status", func() bool { return json.Unmarshal([]byte(out.String()), &st) == nil && len(st.Listen) > 0 })
	addr, err := wire.ParseMultiaddr(st.Listen[0], 1024)
	if err != nil {
		t.Fatal(err)
	}
	relayID, err := wire.ParsePeerID(st.Listen[0][strings.LastIndex(st.Listen[0], "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	if _, err := h.Dial(dctx, addr); err != nil {
		t.Fatal(err)
	}
	c, err := circuit.NewClient(h, circuit.ClientConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Reserve(dctx, relayID)
	if err != nil {
		t.Fatal("reserve:", err)
	}
	if err := circuit.VerifyVoucher(res.Voucher, relayID, h.ID(), res.Expires); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case code := <-done:
		if code != apiv0.ExitOK {
			t.Fatalf("exit %d, stderr %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not stop")
	}
	// The repo lock is released, so a daemon may use the identity again.
	if l, err := r.Lock(); err != nil {
		t.Fatal(err)
	} else {
		l.Release()
	}
}
