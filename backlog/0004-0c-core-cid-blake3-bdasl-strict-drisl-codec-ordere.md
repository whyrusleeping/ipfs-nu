---
id: "0004"
title: '0c: core CID (BLAKE3/BDASL), strict DRISL codec, ordered CAR'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0001"
spec_refs:
    - D1. CIDs = DASL/BDASL CIDs
---

## Description
`core/cid`, `core/drisl`, `core/car`: the pure data-model foundation (no I/O).

- CID: CIDv1 only, base32-lowercase string form, codecs `raw` 0x55 / `dag-cbor` 0x71, multihash **blake3 0x1e with a 32-byte digest only** (BDASL). Reject everything else in the native format. SHA-256 parses (read-only) for bridge use.
- DRISL: a strict encoder/decoder (CBOR/c tag-42 profile: tag 42 only, string map keys, canonical ordering, no indefinite lengths, no undefined, float rules). Struct mapping for Go types, and `$type` handling.
- CAR v1: streaming writer/reader with verification as blocks arrive and a deterministic ordering helper.

**Acceptance**
- DASL/DRISL test suites pass (where applicable to BDASL); round-trip property tests; non-canonical encodings rejected.
- Fuzz targets for the CID parser, DRISL decoder and CAR reader.
- Blocks decode with an independent DAG-CBOR decoder (e.g. go-ipld-prime) in a cross-check test.

## Acceptance criteria

## Work log
