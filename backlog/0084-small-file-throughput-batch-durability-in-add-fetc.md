---
id: "0084"
title: 'Small-file throughput: batch durability in add, fetch and extract (~85 files/s today)'
status: proposed
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs: []
---

## Description
Measured 2026-10-22 on alphac (docs/validation.md "LAN benchmarks"): a 10k × 100 KiB tree takes ~120 s to `nu add` (82–85 files/s, only 4.7 s of CPU), ~90 s to fetch and ~62 s to extract. A second source gives no gain. Each stored blob gets its data and outboard `fsync`ed and committed in its own SQLite transaction (store.putBlob → commitBlob; `--nocopy` drops the data fsync and halves the time). Fetch and extraction show the same ~6–9 ms per-file cost on consumer NVMe.

Proposal: keep crash safety but batch durability. Stage many blobs, fsync them together (or `syncfs`/batched fdatasync) before one metadata transaction that makes them visible and pins the root. Apply the same to fetch completion and extraction. Add/get stays atomic from the user's point of view: nothing is visible or pinned before it is durable.

Acceptance: the same benchmark (`scripts/lan-bench.sh add get1`) shows the tree add and get several times faster (target ≥1000 files/s add on alphac), with big-file numbers unchanged; one crash test (kill during add/fetch, then reopen) shows no visible blob whose bytes are missing.

## Acceptance criteria

## Work log
