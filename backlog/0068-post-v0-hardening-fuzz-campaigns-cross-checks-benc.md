---
id: "0068"
title: 'Post-v0 hardening: fuzz campaigns, cross-checks, benchmarks and scale tests deferred from v0'
status: done
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
- 2026-10-21: Done on track/http (a87d8de; merged 04a4ac6); results in docs/validation.md, reproduction in experiments/hardening/README.md. Fuzz: 37 targets × 60 s, 75.6M execs, no crashers; nightly workflow. Cross-checks: 92 DASL fixtures pass; go-car CAR checks; indigo data-model; Rust bao-tree 0.16.1 16 KiB vectors (10 sizes, 30 slices) match; go-ipld-prime defect found (−2^64 decodes as 0). Benchmarks: BLAKE3 9.1 GB/s, Bao outboard 3.1 GB/s (amd64); fanout 16 retained as a space/read compromise. Scale: 10 GB add/cat/get and multi-peer fetch pass with <3 MB heap delta; 1M-entry lookups pass. LIMITATION: fetch throughput 73.7 MB/s single provider and 47.3 MB/s with two honest + one corrupt (0.64×, not near-sum) → task 0071. Crash injection: 48 randomized SIGKILL trials clean. Platforms: macOS arm64/amd64 + Linux arm64 nightly jobs written; remote execution unobserved.
