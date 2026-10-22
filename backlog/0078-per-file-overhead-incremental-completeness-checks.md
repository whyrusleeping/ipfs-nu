---
id: "0078"
title: 'Per-file overhead: incremental completeness checks (store.Has hot path in replica.check) and SQLite space reclaim'
status: in_progress
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/data.md#D17. Store and replication
    - docs/operations.md#Readiness and status
---

## Description
Found by the 0034 smoke run: updating a 2,000-file `sparse` tree takes ~9 s on one node and ~8 s to `ready` on a 3-member cluster; a CPU profile puts 71% of CPU in per-blob `store.Has` lookups called from `replica.Manager.check` during readiness evaluation and archive setup (each status/wait/readiness pass re-walks the target and checks every blob). Also: after GC the SQLite file doesn't shrink (reclaim falls short of the estimate for small objects), and metadata growth exceeds novel content for small updates.

Work: make completeness checks incremental/cached (e.g. per-target completeness memo invalidated by store changes, batched `Has` over sets of multihashes in one SQL query, reuse of the previous version's verified-complete subtrees via MST diff), and address SQLite space reclaim (incremental_vacuum / auto_vacuum=INCREMENTAL migration, or reporting reclaimable space honestly). Re-run `NU_BENCH_PILOT=sparse,site` before/after.

Acceptance: `sparse` 2,000-file update + ready on one node and on the 3-member cluster each ≥5× faster (or a stated reason), reclaim estimate vs actual within 20% after the fix (or the estimate made honest), numbers recorded in docs/validation.md; suite stays fast.

## Acceptance criteria

## Work log
