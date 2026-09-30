---
id: "0009"
title: '0c: Store: blob + outboard store, embedded DB metadata, pins, GC, --nocopy'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0006"
    - "0007"
    - "0036"
spec_refs:
    - docs/data.md#D17. Store and replication
---

## Description
`store/`: local storage (D17).

- Blob store keyed by multihash: BLAKE3 blobs with Bao outboards, SHA-256 blobs without (D21); small blobs and outboards inline in the DB; `--nocopy` mode that references files in place and re-verifies them (mtime/size check, plus a full re-hash on mismatch).
- An embedded pure-Go DB (choose bbolt vs `modernc.org/sqlite`; record the choice) holding DRISL blocks, pins, partial-blob range maps (for `/nu/have/1`), and a name and peer cache table.
- Pins: recursive with a walk profile (`nu-tree`, `atrepo` with optional blobs and expected dangling links, bounded generic `drisl`) and direct, tagged explicit/follow/my-names/cluster/cluster-surplus. Mirror follows protect held + previous targets; archive retention is explicit common history machinery (0047), not a contradictory generic `keep` policy. Observed name cache and held/served views are separate.
- Enforce logical/unique-physical/transfer limits, staging cleanup and free-space reserve per resolved 0036 contracts. Protected/retained/surplus pins survive disk pressure. Mutable `--nocopy` sources invalidate availability on changes and are not independent archive copies.
- Mark-and-sweep GC with a size watermark; crash-safe (no dangling blobs or partial writes after kill -9).

**Acceptance**
- Add/get of a 10 GB file with constant memory.
- Partial blobs survive restart and resume.
- GC never removes anything reachable from a pin (property test).
- Crash injection and disk-full/false-size/retarget tests preserve held and retained pins; actual resource accounting is bounded, with recoverable pause output. Partial/corrupt no-copy sources are not advertised complete.

## Acceptance criteria

## Work log
