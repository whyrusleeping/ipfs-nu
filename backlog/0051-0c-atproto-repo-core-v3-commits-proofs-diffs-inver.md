---
id: "0051"
title: '0c: atproto repo core: v3 commits, proofs, diffs/inversion, streamable CAR, P-256/k256 signing'
status: done
priority: 2
created: "2026-09-30"
updated: "2026-10-21"
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
- Adds the **atproto v3 MST profile** to 0007's parameterized MST (SHA-256 depth in 2-bit units, `{l, e:[{p,k,v,t}]}`, mandatory prefix compression, canonical empty-node pruning, values are links), byte-exact with atproto, passing the atproto-interop-tests MST vectors and agreeing with indigo on randomized trees. On it: record lookup, inclusion and absence proofs, proof CAR build/verify, repo diffs ("CAR slices" with the blocks needed for inversion), op inversion against `prevData`, and applying a `getRepo?since=` diff to reach a verified new `data`.
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
- 2026-10-21: Done on track/data (2f27fd9, 631086d, 3e81453; merged). core/atrepo + Layered MST profile option + MaxEntries/MaxLayer limits; store registers atrepo and atrepo+blobs walk profiles. Evidence: atproto-interop-tests MST/crypto/firehose vectors (vendored), 4 real getRepo CARs from the TS PDS (blacksky.app) and cocoon verify with blocks byte-identical and stable re-export (~60 more repos up to 174k records checked manually); proof blocks equal getRecord output; experiments/atrepo_indigo_test.go: indigo verifies a P-256 did:key repo and agrees on roots over randomized trees; FuzzDecodeCommit/FuzzMSTNode/FuzzCARSlice; TestKeyMinedLimit. Deviation: whole-file byte identity with getRepo CARs is impossible because PDSes don't emit streamable order; we match per block and drop unreferenced blocks (spec says so). Policy decisions in docs/dasl.md: non-conforming records (e.g. floats) are kept byte-exactly and flagged, not rejected; node entry limit 256. Main module uses only indigo atcrypto + syntax (indigo atproto/repo would pull go-libp2p).
