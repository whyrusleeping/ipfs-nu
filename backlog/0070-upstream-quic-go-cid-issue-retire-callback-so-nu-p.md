---
id: "0070"
title: 'Upstream quic-go: CID issue/retire callback so Nu-path stops paying qlog construction cost'
status: blocked
priority: 3
created: "2026-10-21"
updated: "2026-10-22"
depends_on:
    - "0015"
spec_refs:
    - docs/networking.md#D11. Nu-path
---

## Description
From 0015 profiling: magicsock's virtual-direct path is within ~0% of an identically instrumented raw quic-go transport, but 16–22% below an uninstrumented one, because magicsock observes connection-ID issue/retirement through quic-go's qlog tracer and stock quic-go constructs every qlog event before calling the recorder (~70–78% of allocated bytes in the profile). D11 requires CID tracking, and NEW_CONNECTION_ID/retirement frames are encrypted, so there is no endpoint-only alternative.

Work: propose and land upstream (quic-go) a narrow connection-ID lifecycle callback or selective event subscription; switch magicsock to it once released, staying on stock quic-go (no fork). Alternative only if upstream declines: per-peer Transports / self-routing CIDs (a host/dispatcher architecture change — needs a spec decision).

Acceptance: magicsock virtual-direct throughput within ~5% of uninstrumented raw quic-go at the same packet size, measured as in docs/validation.md (0015 section), on a stock quic-go release.

## Acceptance criteria

## Work log
- 2026-10-22: Experiment done (d893d5f, 98a6106; merged d85fec8): experiments/quicgo-cid has an upstream-ready quic-go patch adding Config.ConnectionIDTracer (AddedConnectionID before send / RemovedConnectionID after retirement grace, exactly once; tests incl. 50 MB rotation integration), setup.sh, a magicsock copy using it, and draft issue/PR text. Benchmark (0015 method, pooled 10 runs): magicsock with the hook 703 MB/s vs uninstrumented raw 726 (−3.2%, within target) vs current qlog observer 568 (−21.8%). Switch plan in docs/networking.md D11. BLOCKED on humans: open the quic-go issue/PR per README (quic-go AI-POLICY requires disclosure and author understanding), then a stock release; then bump quic-go, port the observer, rerun the benchmark. Side observation for upstream (unconfirmed): possible hang in server.go handleInitialImpl when AddWithConnID fails.
