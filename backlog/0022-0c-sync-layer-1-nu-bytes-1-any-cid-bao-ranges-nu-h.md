---
id: "0022"
title: '0c: Sync layer 1: `/nu/bytes/1` (any CID, Bao ranges) + `/nu/have/1`'
status: in_progress
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
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

**Acceptance (v0 cut; deferred evidence is in 0068)**
- A client verifies a 64 MiB blob and arbitrary ranges from a peer (multi-GB in a `NU_SLOW` test).
- A SHA-256 blob transfers and verifies; a reset restarts cleanly; a corrupted transfer is discarded and the provider demoted; peers cannot push more than 8 MiB of unverified whole-object data.
- Cancellation frees server resources.
- Fuzz targets with seeds for the request decoders.
- The protocol description is checked in to docs/data.md.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (ae505e8, merged 4ee694e). proto/ client (Fetch/FetchRanges/FetchWhole/Have, ErrCorrupt vs StatusError vs transport) and server (limits, busy status). Wire: one Bao slice per range in request order; statuses 0–5; have bitmap+info; limits; vectors in testdata/vectors/nu-proto. Evidence: TestBlobRangesAndHave (64 MiB, ranges, onward serving from partial), TestSlowLargeBlob (NU_SLOW 3 GiB, 20.7 s), TestWholeObjects, TestCorruptTransfer, TestCancelAndResume (Active()→0), TestOversizedWholeObject, FuzzBytesRequest/FuzzHaveRequest/FuzzHaveResponse, TestVectors. Spec in docs/data.md D14. Caveat for 0027: Fetch(size=-1) trusts peer-reported size for the partial; pass authenticated sizes and add a discard-partial path.
