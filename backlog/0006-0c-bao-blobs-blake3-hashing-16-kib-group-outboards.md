---
id: "0006"
title: '0c: Bao blobs: BLAKE3 hashing, 16 KiB-group outboards, verified range slices'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
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

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Bao spec test vectors pass.
- A corrupted byte in a chunk group is detected within that group.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (d20d70a). core/blob outboards, ExtractSlice, incremental DecodeSlice with per-group CorruptError. Evidence: TestBaoVectors (standard + abao 16 KiB group vectors), TestRangeSliceAndCorruption. Byte-range→group rule (start rounds down, end up) recorded in docs/data.md D2; the BDASL page's 'round start up' looks like an upstream error — worth raising with DASL.
