---
id: "0053"
title: 'atproto record fetch and repo snapshots: nu get at://, CAR import/export, ls/cat and /at/ gateway browsing'
status: done
priority: 2
created: "2026-09-30"
updated: "2026-10-21"
depends_on:
    - "0009"
    - "0028"
    - "0035"
    - "0051"
spec_refs:
    - docs/dasl.md#Using atproto data
    - docs/names.md#D6. Resolution and caching
---

## Description
Make atproto repositories easy to work with from the Nu CLI, API and gateway (D21 "Using atproto data").

- `nu get at://<did|handle>/<collection>/<rkey>`: resolve identity and fetch `getRecord` with its proof; verify; return JSON (`$link`/`$bytes`) with commit CID and rev. Keep the proof. The D6 name-resolution code is the same path specialised to `io.ipfs.nu.name`.
- `nu get at://<did|handle>`: `getRepo` CAR, then verify the signature, check that the rebuilt MST equals `data`, validate records and pin with `atrepo`. `--blobs` fetches `listBlobs`/`getBlob`, verified as SHA-256 raw CIDs; blobs over 8 MiB are skipped and reported as oversized.
- `nu car import repo.car --profile atrepo` works offline. It verifies structure, and verifies the signature when identity is available or a key is pinned. `nu car export <commit> --profile atrepo` writes streamable order.
- `nu ls`/`cat` on held snapshots and live repos. Gateway `/at/<did>[@<rev>]/<collection>/<rkey>` renders inert JSON/text only; blobs are non-active downloads; public readers only ever see held snapshots.
- Budgets per account and per PDS host; SSRF guards on DID documents and PDS endpoints.

**Acceptance**
- A real Bluesky account snapshot imports, browses and re-exports a CAR that `goat` verifies.
- Tampered CARs, wrong signing keys, foreign-CID records and oversized/key-mined repos are handled with clear errors.
- Gateway tests show record/blob content cannot execute in the gateway origin.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/app (0665c51..a5ce891; merged). atproto/ (ParseURI, Resolver for handles/did:plc/did:web via httpsrc, GetRecord/GetRepo/GetBlob), drisl walk profile, node AtRecord/AtSnapshot/CarImport/CarExport, ls/cat of at:// targets, /at/ gateway routes (inert), CLI nu get at://…, nu car import|export. Evidence: cmd/nu TestCarImportExport (vendored blacksky CAR); goat repo inspect/ls/mst read Nu re-exports incl. a live 30 MB / 85,921-record snapshot; atproto.TestFetch (tampered repo+proof, wrong key, unlisted handle, private PDS refused); node.TestAtprotoSnapshotBrowseCar; gateway.TestAtRoutesInert (N30); NU_LIVE=1 TestLive passes. Deferred: per-account budgets/config section, since-diff refresh (0054), batched CAR ingest Put (WAL amplification), independent review of the 0053 gate. Product questions raised: snapshot retention, ls at:// fetching whole repo, pinning offline-unverified imports.
