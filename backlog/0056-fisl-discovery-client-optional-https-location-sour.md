---
id: "0056"
title: FISL discovery client (optional HTTPS location source)
status: blocked
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
- 2026-10-22: Implemented (d60b24b, 117dd97, 68c9a5a; merged 0034c32). fisl/ package (bounded JSON/JSONL decoders, expiry ceilings, scheme filter, publisher/origin demotion, SSRF via httpsrc) + rasl.Retriever shared URL-level verified retrieval; fisl.indexers config (off by default; never in private/isolated clusters) feeds user fetches. Evidence: fisl.TestDecode, fisl.TestSource (down/HTML/liar indexers, expiry, floods, 1.2 MiB BLAKE3), fisl.TestSSRF, node.TestGetFISL. BLOCKED on external dependency: no public reference FISL indexer exists yet (spec at dasl.ing/fisl.html, 2026-10-21; vmx's CollGS host serves SPA HTML for the path). Opt-in fisl.TestLive (NU_LIVE_FISL=<indexer>,<cid>) is ready for the interop acceptance once one exists.
