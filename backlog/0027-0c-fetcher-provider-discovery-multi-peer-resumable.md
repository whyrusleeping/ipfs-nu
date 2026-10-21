---
id: "0027"
title: '0c: Fetcher: provider discovery, multi-peer resumable blob download, tree fetch'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0026"
    - "0040"
spec_refs:
    - docs/data.md#D15. Content-provider discovery
    - docs/networking.md#D9. Peer sessions
---

## Description
`fetch/`: turns "get this CID" into efficient, verified, multi-peer transfers (D15).

- First slice uses explicit/link/connected/LAN providers via `/nu/have/1` and minimal direct sessions (0040). Discovery/session interfaces admit name context, Amino/delegated routing (0023) and relay/direct path race (0020) when those land; they are not dependencies of the first fetcher.
- Public different-NAT acceptance still requires the relay/session integration in 0029. Do not claim hinted direct-only retrieval is complete zero-config reach.
- **Multi-peer blob download:** parallel ranges across peers, an endgame phase, dropping peers that send bad data (detected per 16 KiB group), and serving completed ranges onward immediately; resumable across restarts (partial blobs in the store).
- Tree fetch: `/nu/tree/1` when a peer supports it; otherwise a layer-1-only walk (one round trip per level). Blobs referenced by a tree are fetched in parallel from multiple peers.
- Session-aware: requests move to the best connection as paths change (D9).

**Acceptance**
- 10 GB from 3 peers, with one peer injecting corrupt data: completes, the corrupt peer is dropped, and throughput is near the sum of honest peers.
- Killing the process mid-download and restarting resumes without re-fetching verified ranges.

## Acceptance criteria

## Work log
