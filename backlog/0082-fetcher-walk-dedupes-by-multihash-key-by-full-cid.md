---
id: "0082"
title: 'Fetcher walk dedupes by multihash: key by full CID so raw+DRISL aliases complete'
status: in_progress
priority: 3
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/data.md#D15. Content-provider discovery
---

## Description
From the 0078 follow-up: `fetch.(*run).walk` dedupes visited objects by multihash. If a tree links the same bytes as raw first and as DRISL second, the fetcher skips the DRISL node, never fetches it as structure, and the target never completes (store/replica now correctly report it incomplete, so this is liveness, not safety). Key the fetch walk's dedupe by full CID (and check the tree/bytes protocol client paths for the same assumption), with a test that such a tree completes.

## Acceptance criteria

## Work log
