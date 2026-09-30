---
id: "0006"
title: '0c: Bao blobs: BLAKE3 hashing, 16 KiB-group outboards, verified range slices'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0004"
spec_refs:
    - docs/data.md#D2. Blobs
---

## Description
`core/blob`: whole-file BLAKE3 blobs with standard Bao verified streaming at 16 KiB chunk groups (D2).

- Hashing + outboard generation (streaming, for files larger than RAM) using `lukechampine.com/blake3/bao`.
- Slice extraction for ranges (in 16 KiB chunk groups) and slice verification on the receiving side, as the data streams.
- The BDASL rules for mapping byte ranges to chunk ranges.
- Blobs ≤ 16 KiB: plain whole-blob hash check, no outboard.
- Bao applies only to BLAKE3 blobs. SHA-256 raw blobs (`--hash sha256`, atproto/DASL imports) have no outboard; they are verified whole and limited to 8 MiB (D21).

**Acceptance**
- Bao spec test vectors pass. **Cross-check vectors with iroh's `bao-tree` at 16 KiB groups** and record whether the encodings are byte-identical (outcome goes into docs/validation.md).
- A corrupted byte in any chunk group is detected within that group.
- Benchmark: hashing and outboard throughput on amd64 and arm64 (target: close to raw BLAKE3 speed).

## Acceptance criteria

## Work log
