package swarm_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/swarm"
)

// gatewayChecker is the IPFS public-gateway checker's well-known file ("Hello from IPFS
// Gateway Checker"), pinned by many gateways and pinning services.
const gatewayChecker = "bafybeifx7yeb55armcsxwwitkymga5xf53dxiarykms3ygqic223w5sk3m"

// Live check (NU_AMINO=1, needs the Internet): a network dry run and an import of a small
// public CID through the product path — Amino DHT bootstrap and provider lookup, then
// Bitswap 1.2 over QUIC-v1 — with every block verified under its original CID.
// NU_AMINO_CID overrides the root.
func TestAminoBitswapImport(t *testing.T) {
	if os.Getenv("NU_AMINO") != "1" {
		t.Skip("set NU_AMINO=1 for the live Amino/Bitswap import check")
	}
	root := gatewayChecker
	if c := os.Getenv("NU_AMINO_CID"); c != "" {
		root = c
	}
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := node.NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := swarm.RepoConfig(r, l.Key, l.Store)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = []string{"/ip4/0.0.0.0/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1"}
	cfg.MDNS = false
	cfg.Logger = slog.New(slog.DiscardHandler)
	sw, err := swarm.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.Fetcher, l.Net = sw, sw
	if err := l.AttachNetwork(sw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.DetachNetwork(); _ = sw.Close(); _ = l.Close() })
	run := func(req apiv0.IPFSImportRequest) apiv0.IPFSImportResult {
		t.Helper()
		start := time.Now()
		o, err := l.ImportIPFS(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		final, err := l.WaitOperation(context.Background(), o.ID, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if final.State != apiv0.StateSucceeded {
			t.Fatalf("%s after %s: %+v", final.State, time.Since(start).Round(time.Millisecond), final.Error)
		}
		var res apiv0.IPFSImportResult
		if err := json.Unmarshal(final.Result, &res); err != nil {
			t.Fatal(err)
		}
		t.Logf("dry_run=%v in %s: %+v", req.DryRun, time.Since(start).Round(time.Millisecond), res)
		return res
	}
	dry := run(apiv0.IPFSImportRequest{Root: root, DryRun: true})
	if dry.Complete == nil || !*dry.Complete {
		t.Fatalf("inventory incomplete: %+v", dry.Missing)
	}
	res := run(apiv0.IPFSImportRequest{Root: root})
	if res.Root == "" || res.Files != dry.Files || res.Bytes != dry.Bytes {
		t.Fatalf("import %+v does not match its inventory %+v", res, dry)
	}
}
