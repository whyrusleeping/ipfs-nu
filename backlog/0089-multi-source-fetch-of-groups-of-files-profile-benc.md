---
id: "0089"
title: 'Multi-source fetch of groups of files: profile, benchmark, optimize'
status: in_review
priority: 2
created: "2026-10-29"
updated: "2026-10-29"
depends_on: []
spec_refs:
    - docs/data.md#/nu/bytes/1
    - 'docs/validation.md#Multi-source fetch: groups of files (2026-10-29)'
---

## Description
User request: profile, benchmark and optimize fetching a large file or a group of files from multiple sources.

Acceptance:
- lan-bench covers groups of files between `tree` and `big` (mid 1,000 × 4 MiB, group 20 × 256 MiB) with optional CPU profiles (`PROF=1`).
- Before/after 1:1 and 2:1 measurements on alphac recorded in docs/validation.md "Multi-source fetch: groups of files", with every output digest matching.
- Found bottlenecks fixed without regressing big/tree; one fast regression test for the fixed behavior.

Result: per-run range lanes (no per-blob endgame duplication; mid 2:1 349→970 MB/s, wire bytes 2.05×→1.06×), small-blob slot released before Put plus size-bounded concurrency (tree 72–82→~320 MB/s). Test: fetch.TestFewChunkBlobsNotDuplicated.

## Acceptance criteria

## Work log
- 2026-10-29: round 2 (user request): transfer-only benchmark `bench/cmd/xferbench` (lan-bench `xfer1`/`xfer2`), `NU_BLOCKPROFILE` (`PROF=2`). Tree recipient was bound by the store's single SQLite connection: SQL group commit for fetched whole blobs (`store.Batch.Sync`), no outboard file for copied blobs ≤ 1 MiB (D17). Tree 2:1 fetch 308–324 → 540–561 MB/s, 1:1 315–324 → 373–392; big/group/mid unchanged. Evidence: docs/validation.md "Transfer only, and the tree again".
- 2026-10-29: round 3 (user decision: `get` streams to disk; a partial left on failure is fine; rename only the top level). `get` extracts while fetching (`store.OpenStream` follows live partials; announcements after commits), new outputs go to `<out>.nu-partial` and are renamed when complete, existing outputs are updated only after the fetch succeeds (X7). Blobs fetched in depth-first order. Big 2:1 total 13.8–14.1 → 12.2–12.9 s (write-out tail 3.2 → ~0.5 s). Tests: store.TestOpenStream, node.TestGetStreamsToPartial. Evidence: docs/validation.md "Streaming `get` (X7)".
- 2026-10-29: checkpoint cadence (user decision): partial checkpoints need 16 MiB and 500 ms since the last began (`fetch.Config.FlushInterval`); workers wait only for a checkpoint older than the interval. Interleaved 2:1 big: fetch 11.6–11.9 → 10.8–11.3 s. Single-copy get and reflink filed as proposed 0090, 0091.
