---
id: "0007"
title: '0c: profiled MST (nu-dir + atproto v3) + directory/tree/MASL file node formats + fanout benchmark'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0004"
spec_refs:
    - docs/data.md#D3. Directory trees
    - docs/data.md#D3a. Single-file nodes
---

## Description
`core/mst` + `core/tree`: deterministic directory trees (D3, D3a).

- One MST implementation with profiles (D21): depth hash, bits per level, node schema and value form are parameters. **atproto v3 profile** (SHA-256 depth in 2-bit units, `{l, e:[{p,k,v,t}]}`, mandatory prefix compression, canonical empty-node pruning, values are links) must match atproto byte for byte. **Nu directory profile**: SHA-256 depth in 4-bit units (fanout 16), inline entries. Insert/delete/lookup/range/prefix scans, structural diff between two roots, inclusion/absence proofs. Nodes are SHA-256 DRISL blocks; enforce the 1 MiB node cap and depth/entry-count limits on untrusted input.
- Node formats: directory map entries `{kind, cid, size, mode?, mime?}` (size = total bytes below), tree header `io.ipfs.nu.tree`, file node as a single-mode MASL document `{ $type: "ing.dasl.masl", src, content-type?, "io.ipfs.nu": {size, name?, mode?} }` (wrapped single files; `--raw` bypass). Blob entries may be BLAKE3 or SHA-256 raw CIDs.
- Builder: walk a local directory → blobs + tree (with the Bao task for blobs).
- **D3 benchmark:** fanout 16 vs 32 (and atproto's 4 as a baseline) on real trees: a Linux kernel checkout, a large node_modules, a synthetic 1M-entry directory. Measure depth, node size distribution, lookup bytes and round trips, diff size after small edits. Record the chosen fanout in the spec.

**Acceptance**
- Insertion-order independence: property tests show random insert/delete orders give identical roots.
- Our own published test vectors (for other implementations) are written.
- Lookup in a 1M-entry directory touches O(log n) nodes (checked in a test).
- Fuzz the node decoder.
- The atproto profile passes the atproto-interop-tests MST vectors (key depths, tree shapes, root CIDs) and agrees with indigo on randomized trees.

## Acceptance criteria

## Work log
