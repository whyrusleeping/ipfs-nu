---
id: "0042"
title: Design and implement verified one-way IPFS content and installation migration
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
depends_on:
    - "0009"
    - "0026"
spec_refs:
    - docs/migration.md#IPFS adoption and migration
    - Milestones
---

## Description
Build optional local Kubo/IPFS Cluster inventory and remote UnixFS import path per migration contract. Specify supported UnixFS/DAG profile, original-block verification, metadata/symlink handling, policy mapping, old-root→Nu-root content-addressed provenance manifest authenticated by the signed version's imported reference, checkpoints and cutover report. Enable importer in the standard Linux/macOS production distribution; minimal embedders may omit it with explicit feature reporting. Reuse Nu store/transfer/replication after conversion; no Kubo API or bidirectional/native Bitswap-serving promise.

Acceptance: dry run, resumable local pinset and remote content import, rejection of corrupt/unsupported originals, metadata transformation reports, no source mutation; fresh Nu-client retrieval and exact-policy readiness before cutover recommendation. Importer/provenance and authority migration tested separately. Follows useful previews, required before recommending existing IPFS deployments migrate.

## Acceptance criteria

## Work log
- 2026-10-22: Importer merged (7d08a1d..ce449ae; merge e6358a0). bridge/: dag-pb/UnixFS (+HAMT) decoders, CIDv0/v1, per-block verification, conversion to BLAKE3 blobs + D3 trees (symlinks, exec mode, MASL for single files), pinned DRISL provenance manifest, disk-backed CAR source with dry-run, durable import operations (/imports, resume with same ID), Bitswap 1.2 client over QUIC + Amino provider lookup, nu import ipfs <cid> [--car]. Evidence: offline bridge tests (tamper, retry, fuzz seeds), NU_DOCKER Kubo v0.40.0 CAR imports (CIDv0/v1, HAMT, raw leaves, 3 MiB, symlinks, modes) compared with ipfs get, Kubo Bitswap 1 MiB block over QUIC-v1. Remaining for acceptance: network-source dry-run, per-block checkpoint resume for CAR jobs, live Amino provider discovery for imports, pinset reconciliation report + exact-policy readiness before a cutover recommendation, signed dataset 'imported' linkage.
