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
- Pins: recursive with a walk profile (`nu-tree`, `atrepo` with optional blobs and expected dangling links, bounded generic `drisl`) and direct, tagged explicit/follow/my-names/cluster/cluster-surplus/collection-select/collection-manual/collection-conflict. Mirror follows protect held + previous targets; archive retention is explicit common history machinery (0047), not a contradictory generic `keep` policy. Observed name cache and held/served views are separate.
- Mutable `--nocopy` sources invalidate availability on changes and are not independent archive copies. Logical/physical/transfer budgets, staging limits and free-space reserve are 0064; keep the store's interfaces able to report per-pin and unique bytes for it.
- Mark-and-sweep GC with a size watermark; crash-safe (no dangling blobs or partial writes after kill -9).

**Acceptance**
- Add/get of a 10 GB file with constant memory.
- Partial blobs survive restart and resume.
- GC never removes anything reachable from a pin (property test).
- Crash injection and disk-full/false-size/retarget tests preserve held and retained pins; actual resource accounting is bounded, with recoverable pause output. Partial/corrupt no-copy sources are not advertised complete.

## Acceptance criteria

## Work log
