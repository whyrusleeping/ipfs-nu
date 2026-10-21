---
id: "0026"
title: '0c: Sync layer 2: `/nu/tree/1` (paths, have-diffs, name ranges, inline, resumable)'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0022"
    - "0007"
    - "0027"
spec_refs:
    - docs/data.md#D14. Transfer protocols
    - docs/data.md#D3. Directory trees
---

## Description
Layer 2 of the sync protocols (D14): the optional accelerator. Never needed for correctness.

- **`/nu/tree/1`** request `{root, path?, name_range?, limit?, scope: node|entry|subtree, have?: [roots], inline_max?, cursor?}` → a stream of DRISL nodes in fixed pre-order (the chain of directory nodes along `path`, then the subtree), each verified by hash on arrival.
- **`have` diffs:** walk the requested and `have` trees side by side and skip identical subtrees; if the server lacks `have`, the client descends and fetches only changed nodes.
- `name_range` + `limit` for paging and multi-peer splitting (with boundary proofs); `inline_max` inlines small blobs (≤ 16 KiB, whole-hash checked); file nodes with `scope: entry` stream/inline their blob.
- Bounded responses (node/byte budget); resume without server state (the client computes the cursor from the last verified node).
- Walk profiles: `nu-tree`, and `atrepo` (commit → MST → records; `have` = held commits; repo-path paging), used by cluster state repos and atproto datasets (D21).
- Extend the protocol spec document.
- Plug into the fetcher (0027) as the preferred tree path when the peer supports it, keeping the layer-1 walk as fallback.

**Acceptance**
- A lookup in a 1M-entry directory with `scope: entry` transfers O(log n) nodes in one request.
- A small edit to a 10k-file site transfers only the changed nodes + new blobs when `have` is given.
- A resume after a mid-stream reset loses nothing and duplicates at most one node.
- Fuzz the decoders.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (21bae0a, b34fa66; merged 13f5525). /nu/tree/1 server+client with framed verified pre-order stream, have diffs, name_range boundary nodes, inline blobs ≤16 KiB, stateless cursors, nodes batch mode; fetcher prefers it with layer-1 fallback. Evidence: TestTreeLookup (20k) + TestSlowTreeLookup1M (NU_SLOW, 10.7 s, one request, O(log n) nodes), TestTreeHaveDiff (10k-file site: diff = exactly changed objects), TestTreeResume (no loss/duplication), FuzzTreeRequest/FuzzTreeStream, TestTreeStreamCorrupt, TestTreeRangeAndNodes, fetch/TestTreeProtocolAndFallback. Deferred: atrepo profile for /nu/tree/1 (plan in docs/data.md), layer-1 walk using nodes batches, client re-validation of have-marked subtrees. Follow-up: nutest Corrupt only wraps /nu/bytes/1.
