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
