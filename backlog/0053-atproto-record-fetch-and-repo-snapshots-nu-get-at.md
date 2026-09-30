---
id: "0053"
title: 'atproto record fetch and repo snapshots: nu get at://, CAR import/export, ls/cat and /at/ gateway browsing'
status: todo
priority: 2
created: "2026-09-30"
updated: "2026-09-30"
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
