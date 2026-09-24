---
id: "0007"
title: '0c: MST with arbitrary keys + directory/tree/file node formats + fanout benchmark'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0004"
spec_refs:
    - 'D3. Trees: one DRISL node per directory, MST, no sharding threshold'
    - D3a. Single files are wrapped (Q30)
---

## Description
`core/mst` + `core/tree`: deterministic directory trees (D3, D3a).

- A general MST: arbitrary UTF-8 keys, configurable fanout (bits of leading zeros per level; atproto's rule generalised), insert/delete/lookup/range/prefix scans, structural diff between two roots. Nodes are DRISL blocks hashed with BLAKE3; enforce the 1 MiB node cap.
- Node formats: directory map entries `{kind, cid, size, mode?, mime?}` (size = total bytes below), tree header `io.ipfs.nu.tree`, file node `io.ipfs.nu.file` (wrapped single files; `--raw` bypass).
- Builder: walk a local directory → blobs + tree (with the Bao task for blobs).
- **Q27 benchmark:** fanout 16 vs 32 (and atproto's 4 as a baseline) on real trees: a Linux kernel checkout, a large node_modules, a synthetic 1M-entry directory. Measure depth, node size distribution, lookup bytes and round trips, diff size after small edits. Record the chosen fanout in the spec.

**Acceptance**
- Insertion-order independence: property tests show random insert/delete orders give identical roots.
- Our own published test vectors (for other implementations) are written.
- Lookup in a 1M-entry directory touches O(log n) nodes (checked in a test).
- Fuzz the node decoder.

## Acceptance criteria

## Work log
