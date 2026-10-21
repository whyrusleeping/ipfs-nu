---
id: "0004"
title: '0c: core DASL/BDASL CIDs, strict DRISL codec, DASL CAR'
status: done
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0001"
spec_refs:
    - docs/data.md#D1. CIDs
    - docs/dasl.md#D21. DASL conformance
---

## Description
`core/cid`, `core/drisl`, `core/car`: the pure data-model foundation (no I/O). See docs/dasl.md (D21).

- CID: CIDv1, `b` + lowercase base32, codecs `raw` 0x55 / DRISL 0x71, multihash **sha2-256 0x12 or blake3 0x1e, 32-byte digests only** (DASL + BDASL). All four combinations are first-class. Nu produces SHA-256 for DRISL structure, and BLAKE3 (default) or SHA-256 for raw blobs; it never produces BLAKE3 DRISL. Reject everything else as a block CID. Keep foreign CIDs found inside record contents as opaque links (atproto import profile).
- Hashing API keyed by multihash. Enforce the fixed 8 MiB whole-object limit (all SHA-256 objects, BLAKE3 DRISL) in the CID/CAR/DRISL entry points.
- DRISL: strict encoder/decoder (CBOR/c tag-42 profile: tag 42 only with a 0x00 prefix, string map keys, length-first canonical order, minimal ints/lengths, no indefinite lengths, only false/true/null simple values, 64-bit floats only with no NaN/±Inf/−0, no trailing bytes). Nesting/memory bounds. Struct mapping for Go types and `$type` handling. atproto data-model validation mode (no floats, blob refs, legacy blob refs).
- JSON: atproto `$link`/`$bytes` conventions both ways.
- CAR v1 (DASL CAR): streaming reader/writer, verifying every block as it arrives. Full header metadata exposed (MASL-compatible headers). Tolerate duplicates and arbitrary order; report unreferenced blocks. Reject CARv2 with an explanation. Ordering helpers for Nu tree pre-order and atproto streamable order.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- CID and DRISL vectors from dasl-testing (BDASL cases included) and the atproto data-model vectors pass, vendored under `testdata/vectors/`.
- Non-canonical encodings are rejected (table tests); a small round-trip property test.
- Fuzz targets with seeds for the DRISL decoder and CAR reader.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/data (a9f578c, merged 2358a5c). Hand-rolled strict core/cid; core/drisl (limits, struct mapping, $type, atproto mode, $link/$bytes JSON); core/car streaming verified reader/writer, CARv2 rejection, PreOrder helper. Evidence: TestDASLTestingVectors (92 cases incl. BDASL), TestAtprotoFixtures, TestRejectNonCanonical, TestRoundTripProperty, FuzzDecode, FuzzReader. JSON rules recorded in docs/dasl.md.
