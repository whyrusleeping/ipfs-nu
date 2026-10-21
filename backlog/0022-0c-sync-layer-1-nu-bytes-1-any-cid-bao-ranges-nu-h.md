---
id: "0022"
title: '0c: Sync layer 1: `/nu/bytes/1` (any CID, Bao ranges) + `/nu/have/1`'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0008"
    - "0009"
spec_refs:
    - docs/data.md#D14. Transfer protocols
---

## Description
Layer 1 of the sync protocols (D14), with binary DRISL framing.

- Framing: uvarint-length-prefixed DRISL request → DRISL response header `{status, error?, size?, meta?}` → body; one stream per request; cancel = reset; unknown keys ignored; hard limits (request size, concurrent requests per peer and per protocol).
- **`/nu/bytes/1`**: `{mh, ranges?}` for **any** DASL/BDASL object, keyed by multihash. BLAKE3 → a Bao slice streamed from the store, and partial blobs serve the ranges they hold. Whole-object-verified objects (SHA-256, BLAKE3 DRISL; ≤ 8 MiB) → sent whole, verified by the client at the end and never served onward before then. Oversized requests are refused (D21).
- **`/nu/have/1`**: batched `{cids[], range queries}` → bitmap + `{codec, size}` + held ranges (range list; bitmap when fragmented, per D14). Only ever sent to candidate peers, never broadcast.
- Write a short protocol spec document (messages, errors, limits) alongside the code, so other implementations are possible.

**Acceptance**
- A client verifies a 10 GB blob and arbitrary ranges from a peer.
- A SHA-256 blob and atproto repo blocks transfer and verify; a reset restarts cleanly; a corrupted transfer is discarded and the provider demoted; peers cannot push more than 8 MiB of unverified whole-object data.
- Fuzz the request decoders.
- Cancellation frees server resources immediately.
- The protocol spec doc is checked in.

## Acceptance criteria

## Work log
