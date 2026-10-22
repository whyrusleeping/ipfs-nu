---
id: "0074"
title: Test suite speed and robustness under load (keep go test ./... under a minute, no load flakes)
status: todo
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - CONTRIBUTING.md
---

## Description
Since v0 the per-PR suite (`go test ./...`) has grown many multi-node scenario tests (cluster, collection, nutest, node, proto, bridge). On the shared 192-core dev machine under load (load avg 20–30 from concurrent agents and multi-GB benchmarks) several tests time out on tight wall-clock deadlines (e.g. `p2p/lookup.TestCacheHintsRestartParallel` 1 s, `cluster.TestPrivateCluster`, `TestDrainRemove`, `nutest.TestRestartMidTransfer` startup, `fetch.TestMultiPeerWithCorruptPeer`, `proto.TestBlobRangesAndHave`, `node/daemon.TestDaemonEndToEnd`), and packages like nutest/proto/node took 100–200 s under load. CONTRIBUTING.md requires the suite to stay well under about a minute, with no sleeps, waiting on conditions with timeouts, and says flaky tests are fixed or deleted.

Work: measure per-package and per-test time on an idle run and under load (e.g. `go test -json` + a small report script); make deadlines condition-based with generous ceilings that only catch hangs; cut redundant multi-node scenarios or move heavy ones behind `NU_SLOW` / nightly where they duplicate coverage; check `-p`/`GOMAXPROCS` interactions (192 cores → many parallel packages each spinning QUIC hosts); record before/after in docs/validation.md.

Acceptance: idle `go test ./...` under ~60 s wall; 5 consecutive full runs green while the machine is under comparable load (or with `stress-ng`-style CPU/IO load if available), with no test weakened to hide a real failure.

## Acceptance criteria

## Work log
