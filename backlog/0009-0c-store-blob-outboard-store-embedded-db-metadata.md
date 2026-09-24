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
spec_refs:
    - D17. Store and replication
---

## Description
`store/`: local storage (D17).

- Blob store: data files keyed by CID plus Bao outboards; small blobs and outboards inline in the DB; `--nocopy` mode that references files in place and re-verifies them (mtime/size check, plus a full re-hash on mismatch).
- An embedded pure-Go DB (choose bbolt vs `modernc.org/sqlite`; record the choice) holding DRISL blocks, pins, partial-blob range maps (for `/nu/have/1`), and a name and peer cache table.
- Pins: recursive (trees/file nodes) and direct (blobs), tagged by source (explicit / follow / my-names), with policy `keep: latest | N | all` (default: previous version of each followed name, per Q28).
- Mark-and-sweep GC with a size watermark; crash-safe (no dangling blobs or partial writes after kill -9).

**Acceptance**
- Add/get of a 10 GB file with constant memory.
- Partial blobs survive restart and resume.
- GC never removes anything reachable from a pin (property test).
- A crash-injection test passes.

## Acceptance criteria

## Work log
