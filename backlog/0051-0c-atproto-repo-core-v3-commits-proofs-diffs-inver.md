---
id: "0051"
title: '0c: atproto repo core: v3 commits, proofs, diffs/inversion, streamable CAR, P-256/k256 signing'
status: todo
priority: 1
created: "2026-09-30"
updated: "2026-09-30"
depends_on:
    - "0004"
    - "0007"
spec_refs:
    - docs/dasl.md#atproto repository profile
    - docs/dasl.md#Walk profiles
    - docs/names.md#D6. Resolution and caching
---

## Description
`core/` atproto repository profile (D21), with no I/O. Used by name proofs (D6), `at://` record fetch, repo snapshots, atproto archive datasets and cluster state repos (D20).

- Commit v3 `{did, version: 3, data, rev, prev: null, sig}`: build, sign and verify with P-256 and secp256k1 (`did:key` and DID-document keys). Reject future revs beyond the drift window.
- Uses the atproto MST profile from 0007: record lookup, inclusion and absence proofs, proof CAR build/verify, repo diffs ("CAR slices" with the blocks needed for inversion), op inversion against `prevData`, and applying a `getRepo?since=` diff to reach a verified new `data`.
- Full-repo verification: rebuild the MST from a CAR, check it equals `data`, and validate every record under the atproto data model. Records may keep foreign/legacy CIDs as opaque links.
- Streamable CAR export order (commit, then MST pre-order with records interleaved); import tolerates any order and duplicates.
- `atrepo` walk profile (commit → MST → records, optional blob refs, expected dangling links).
- Untrusted-input limits: entries per node, depth, record size (1,000,000 bytes), total blocks.
- Prefer indigo's `atproto/repo`, `atcrypto` and `atdata` packages where they fit. Otherwise implement it ourselves and cross-check against indigo.

**Acceptance**
- The atproto-interop-tests MST, crypto, data-model and firehose (commit/inversion) vectors pass.
- Byte-identical round-trip of real `getRepo` CARs (several PDS implementations); proofs match `getRecord` output.
- A cluster-state-style repo signed by a P-256 `did:key` exports a CAR that `goat` (or indigo) verifies.
- Fuzz the commit/MST/CAR-slice decoders; malicious key-mined trees stay within limits.

## Acceptance criteria

## Work log
