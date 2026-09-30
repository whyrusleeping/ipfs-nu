---
id: "0042"
title: Design and implement verified one-way IPFS content and installation migration
status: todo
priority: 2
created: "2026-09-29"
updated: "2026-09-29"
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
