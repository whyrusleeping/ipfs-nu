---
id: "0011"
title: '0a exit: libp2p interop CI (unified-testing quic-v1, Kubo, Ethereum consensus client)'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0010"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
    - docs/networking.md#D16. Testing and diagnostics
---

## Description
This is the Phase 0a exit gate: prove wire compatibility continuously.

- Integrate the libp2p `unified-testing` (formerly test-plans) **transport interop suite for `quic-v1`** against go-, rust- and js-libp2p, in CI.
- Black-box peers in CI (containers): **Kubo** (connect, identify, ping) and at least one **Ethereum consensus client** (e.g. Lighthouse or Prysm) over QUIC with its secp256k1 identity (connect, identify, ping).
- A nightly job against a pinned set of public Amino DHT bootstrap peers (connect + identify only).

**Acceptance (Phase 0a exit)**
- The quic-v1 interop matrix is green for dial and listen in both directions.
- Kubo and the Ethereum client pass connect/identify/ping.
- Results are published as a CI artifact.

## Acceptance criteria

## Work log
