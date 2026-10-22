---
id: "0056"
title: FISL discovery client (optional HTTPS location source)
status: in_progress
priority: 3
created: "2026-09-30"
updated: "2026-10-22"
depends_on:
    - "0052"
    - "0027"
spec_refs:
    - docs/dasl.md#FISL
    - docs/data.md#D15. Content-provider discovery
---

## Description
Optional FISL discovery source (D15/D21): query configured indexers at `GET /routing/locations/{cid}/json|jsonl`; use supported `https` addrs; honour record and addr expiry; parse JSONL incrementally with bounds; treat `size` as unverified; demote failing publishers/origins; fetch via `httpsrc` and verify by CID. Private/isolated clusters never query public indexers.

FISL is "hot new stuff" upstream. Track spec changes and keep this behind configuration. FISL serving and a Nu endpoint URL scheme are extension work.

**Acceptance**: interop with a reference FISL indexer (e.g. Datadisco); expired/hostile/huge responses handled; SSRF tests shared with 0052.

## Acceptance criteria

## Work log
