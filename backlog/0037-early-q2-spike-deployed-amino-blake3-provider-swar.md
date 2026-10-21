---
id: "0037"
title: 'Early Q2 spike: deployed Amino BLAKE3 provider/swarm keys and routing citizenship'
status: todo
priority: 2
created: "2026-09-29"
updated: "2026-10-21"
depends_on:
    - "0001"
spec_refs:
    - docs/validation.md#Early experiments
    - docs/data.md#D15. Content-provider discovery
---

## Description
Harness code lives in the separate `experiments/` Go module (may use go-libp2p/Kubo), never the main module.

Run bounded provider announce/query experiments against deployed Amino implementations for actual BLAKE3 content CIDs and deterministic dataset swarm keys. Check namespace/validation behavior and consequences of advertising providers that do not speak Bitswap. Select/document delegated-routing or Nu protocol-ID fallback if needed.

Acceptance: reproducible experiment harness, positive/negative results and server versions recorded in docs/validation.md; explicit acceptance/citizenship decision; no dependence of hint/LAN/private-member retrieval on outcome; rate-limited disposable records. This spike retires uncertainty before full Kademlia task 0023; not a production routing implementation.

## Acceptance criteria

## Work log
