---
id: "0070"
title: 'Upstream quic-go: CID issue/retire callback so Nu-path stops paying qlog construction cost'
status: todo
priority: 3
created: "2026-10-21"
updated: "2026-10-21"
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
