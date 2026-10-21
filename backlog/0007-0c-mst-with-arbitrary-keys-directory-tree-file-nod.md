---
id: "0007"
title: '0c: profiled MST (nu-dir + atproto v3) + directory/tree/MASL file node formats'
status: done
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0004"
spec_refs:
    - docs/data.md#D3. Directory trees
    - docs/data.md#D3a. Single-file nodes
---

## Description
`core/mst` + `core/tree`: deterministic directory trees (D3, D3a).

- One MST implementation with profiles (D21): depth hash, bits per level, node schema and value form are parameters. Implement only the **Nu directory profile** here, keeping the parameters real so 0051 can add the byte-exact atproto v3 profile without restructuring. **Nu directory profile**: SHA-256 depth in 4-bit units (fanout 16), inline entries. Insert/delete/lookup/range/prefix scans, structural diff between two roots, inclusion/absence proofs. Nodes are SHA-256 DRISL blocks; enforce the 1 MiB node cap and depth/entry-count limits on untrusted input.
- Node formats: directory map entries `{kind, cid, size, mode?, mime?}` (size = total bytes below), tree header `io.ipfs.nu.tree`, file node as a single-mode MASL document `{ $type: "ing.dasl.masl", src, content-type?, "io.ipfs.nu": {size, name?, mode?} }` (wrapped single files; `--raw` bypass). Blob entries may be BLAKE3 or SHA-256 raw CIDs.
- Builder: walk a local directory → blobs + tree (with the Bao task for blobs).
- Fanout is 16 (D3). The fanout benchmark that validates it is deferred to 0068; do not run it here.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Insertion-order independence: a property test (bounded iterations, runs in about a second) shows random insert/delete orders give identical roots.
- A small set of Nu directory-profile vectors is checked in under `testdata/vectors/` (they double as regression tests).
- Lookup in a 100k-entry directory touches O(log n) nodes.
- Fuzz target with seeds for the node decoder.
- atproto MST profile vectors and indigo agreement are verified in 0051, which first needs that profile.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (024023f). core/mst profiled MST (Get/Put/Delete/Walk/Prefix/Diff/Prove/VerifyProof), core/tree formats + BuildFS over fs.FS. Evidence: TestOrderIndependence, TestVectors (testdata/vectors/nu-tree), TestLookup100k (≤5 nodes), TestDiffAndProofs, FuzzNodeDecode, FuzzDirNode. Node encoding, name/mode/symlink/size rules decided in docs/data.md D3. Range/boundary proofs not yet (needed by 0026).
