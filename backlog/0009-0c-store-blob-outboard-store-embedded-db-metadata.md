---
id: "0009"
title: '0c: Core store: blob + outboard store, SQLite metadata, pins, GC, --nocopy'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0006"
    - "0007"
spec_refs:
    - docs/data.md#D17. Store and replication
---

## Description
`store/`: local storage (D17).

- Blob store keyed by multihash: BLAKE3 blobs with Bao outboards, SHA-256 blobs without (D21); small blobs and outboards inline in the DB; `--nocopy` mode that references files in place and re-verifies them (mtime/size check, plus a full re-hash on mismatch).
- SQLite via `modernc.org/sqlite` (decided) holding DRISL blocks, pins, partial-blob range maps (for `/nu/have/1`), and name and peer cache tables. Schema version + forward-migration framework from the first commit.
- Pins: recursive with a walk profile (`nu-tree` for v0; the interface admits `atrepo` from 0051 and bounded generic `drisl` later) and direct, tagged explicit/follow/my-names/cluster/cluster-surplus/collection-select/collection-manual/collection-conflict. Mirror follows protect held + previous targets; archive retention is explicit common history machinery (0047), not a contradictory generic `keep` policy. Observed name cache and held/served views are separate.
- Mutable `--nocopy` sources invalidate availability on changes and are not independent archive copies. Logical/physical/transfer budgets, staging limits and free-space reserve are 0064; keep the store's interfaces able to report per-pin and unique bytes for it.
- Mark-and-sweep GC with a size watermark; crash-safe (no dangling blobs or partial writes after kill -9).

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Blob writes stream (no whole-file buffering); a 64 MiB add/get test checks it; a `NU_SLOW` test covers multi-GB files.
- Partial blobs survive restart and resume.
- GC never removes anything reachable from a pin (bounded property test).
- Objects become visible atomically: one test kills a write midway and finds no partial object after reopening.
- Partial/corrupt no-copy sources are not advertised complete.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (f4edbc3). store/ with SQLite (user_version migrations), blob+outboard files, partial blobs, pins with sources/walk profiles, GC with grace, no-copy. Evidence: TestAddGet64MiB (peak heap), TestAddGetMultiGB (NU_SLOW, 3 GiB, 24 s), TestPartialResume, TestGCKeepsPinned, TestKillMidWrite (SIGKILL child), TestNoCopy. Gap: outboards are always files (only blobs ≤16 KiB are inline) — a storage optimization, deferred. Name/peer cache tables are left for their owners as new migrations; unique-bytes accounting is 0064.
