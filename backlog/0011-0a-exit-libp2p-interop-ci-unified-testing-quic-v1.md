---
id: "0011"
title: '0a exit: libp2p interop CI (unified-testing quic-v1, Kubo, Ethereum consensus client)'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
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
- 2026-10-21: Harness merged (186e6fd; 7d140af): interop/ separate module, Nu unified-testing driver/image, Docker runner with pinned upstream binaries, Kubo/Lighthouse probes, opt-in Amino checks, .github/workflows/interop.yml (manual + nightly, artifacts). Local results: Nu↔go-libp2p and Nu↔rust-libp2p quic-v1 PASS both directions; Kubo v0.40.0 connect/identify/ping PASS; Lighthouse v8.0.1 connect/identify/eth2 SSZ ping PASS (secp256k1 identity; Lighthouse lacks /ipfs/ping); pinned public Amino peers connect/identify PASS. JS: stock unified-testing js manifest lacks QUIC-v1; follow-up in progress with js-libp2p + @chainsafe/libp2p-quic. Remote CI not yet observed (no GitHub remote).
- 2026-10-21: Done (a7b6594; merged). JS rows real: js-libp2p 3.5.1 + @chainsafe/libp2p-quic 2.1.5 in interop/js. Local Docker matrix quic-v1: Nu↔go, Nu↔rust, Nu↔js all PASS in both directions; Kubo and Lighthouse probes PASS; results artifacts under interop/results (CI uploads them). Lodestar v1.50.0 opt-in probe: QUIC + secp256k1 auth OK, but Lodestar's peer manager rejects every non-secp256k1 remote identity (Nu uses Ed25519) — an identity-policy question raised to the user, not a wire bug. Remote GitHub execution not yet observed (no remote).
