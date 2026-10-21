---
id: "0071"
title: 'Fetch/serve throughput: single-provider 74 MB/s and multi-peer slower than single'
status: in_progress
priority: 2
created: "2026-10-21"
updated: "2026-10-21"
depends_on: []
spec_refs:
    - docs/data.md#D14. Transfer protocols
    - docs/data.md#D15. Content-provider discovery
---

## Description
0068's scale run found the transfer pipeline far below its components: a 10 GB fetch from one provider ran at 73.7 MB/s on loopback, and from two honest + one corrupting provider at 47.3 MB/s (0.64× single), while BLAKE3 hashes at 9 GB/s, Bao outboards at 3 GB/s and stock QUIC on loopback does several hundred MB/s. Gate 1 (0029) needs a 10 GB / 10k-file tree between laptops and a 3-peer verified download.

Work: profile `/nu/bytes/1` serve + `fetch/` + store partial writes end to end (CPU, block, mutex profiles; syscalls/fsync, SQLite partial-map updates, flush cadence, request size/chunking, per-peer concurrency, QUIC flow-control windows, copy count), fix the bottlenecks, and make multi-peer scale with providers instead of degrading.

Acceptance: measured (NU_SLOW, method recorded in docs/validation.md): single-provider loopback fetch of a multi-GB blob at ≥ 5× the 73.7 MB/s baseline or within 2× of raw quic-go stream throughput on the same machine, whichever is lower, with a stated reason if neither is reachable; 3 providers (one corrupting) on separately rate-limited paths reach ≥ 0.8× the sum of the honest providers' limits; no regression in verification, resume or memory bounds (existing tests stay green).

## Acceptance criteria

## Work log
