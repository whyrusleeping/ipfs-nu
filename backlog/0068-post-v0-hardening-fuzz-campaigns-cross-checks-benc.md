---
id: "0068"
title: 'Post-v0 hardening: fuzz campaigns, cross-checks, benchmarks and scale tests deferred from v0'
status: in_progress
priority: 2
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0028"
spec_refs:
    - Milestones
    - docs/validation.md#Release evidence
---

## Description
Evidence deliberately cut from the v0-path tasks so v0 ships fast. None of it gates v0; all of it gates the public preview (0029) and the CollGS preview (0063). Do it after v0, in parallel with gate-1/2a work; split by area if useful.

- **Fuzzing campaigns:** nightly CI job running every fuzz target for a bounded time (e.g. 10 min each), with crashers turned into regression seeds. (from 0001, 0002, 0004, 0005, 0008, 0010, 0022)
- **Cross-implementation checks:** full DASL test suite incl. CAR and BDASL cases with a published compliance matrix; independent DAG-CBOR decode (go-ipld-prime) and indigo data-model cross-checks (0004); Bao vectors against iroh `bao-tree` at 16 KiB groups, result in docs/validation.md (0006).
- **Benchmarks:** BLAKE3/Bao throughput on amd64/arm64 (0006); MST fanout 16 vs 32 vs 4 on real trees (kernel checkout, node_modules, synthetic 1M), recording whether fanout 16 holds (0007).
- **Scale and soak:** 10 GB add/cat/get with constant memory (0009, 0022, 0065); 10 GB from 3 peers with a corrupting peer at near-sum throughput (0027); 1M-entry directory lookups (0007).
- **Store crash-injection campaign:** randomized kill points during add/pin/GC/switch, verifying no dangling or partial objects (0009).
- **Platform:** native macOS arm64 + amd64 and Linux arm64 test runs (0001, 0065).

Wire interop beyond the single go-libp2p smoke tests stays in 0011.

**Acceptance:** each item above has a result (pass, fixed, or recorded limitation in docs/validation.md); slow jobs run nightly or on demand, never in the per-PR suite.

## Acceptance criteria

## Work log
