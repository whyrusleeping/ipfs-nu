---
id: "0022"
title: '0c: Sync layer 1: `/nu/bytes/1` (any CID, Bao ranges) + `/nu/have/1`'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0008"
    - "0009"
spec_refs:
    - D14. Sync protocols (replacing Bitswap / Graphsync for Nu data)
---

## Description
Layer 1 of the sync protocols (D14), with binary DRISL framing.

- Framing: uvarint-length-prefixed DRISL request → DRISL response header `{status, error?, size?, meta?}` → body; one stream per request; cancel = reset; unknown keys ignored; hard limits (request size, concurrent requests per peer and per protocol).
- **`/nu/bytes/1`**: `{hash, ranges}` for **any** CID (raw or DRISL, by BLAKE3 digest) → a Bao slice streamed from the store; partial blobs serve the ranges they hold.
- **`/nu/have/1`**: batched `{cids[], range queries}` → bitmap + `{codec, size}` + held ranges (range list; bitmap when fragmented, per Q6). Only ever sent to candidate peers, never broadcast.
- Write a short protocol spec document (messages, errors, limits) alongside the code, so other implementations are possible.

**Acceptance**
- A client verifies a 10 GB blob and arbitrary ranges from a peer.
- Fuzz the request decoders.
- Cancellation frees server resources immediately.
- The protocol spec doc is checked in.

## Acceptance criteria

## Work log
