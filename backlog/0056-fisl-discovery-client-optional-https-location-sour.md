---
id: "0056"
title: FISL discovery client (optional HTTPS location source)
status: done
priority: 3
created: "2026-09-30"
updated: "2026-11-11"
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

## Outcome

Unblocked: a public FISL indexer now exists (index.content-router.com, fed by guillaumemichel/cidsync). fisl.TestLive passed against it for bafyreib4mkuic6epif3kssxiamqv3ujxjsuwixbbjf5zu6ptumdfj67rty (3,641-byte DRISL descriptor from did:web:anymirror.michel.id): lookup, fetch from publisher URL, CID verification; `range` extension field ignored. BLAKE3 retrieval not exercised live (only listed BLAKE3 CID is 138 GB). Recorded in docs/dasl.md#FISL.

Commit: docs: FISL client interop with the public index.content-router.com indexer (0056)
