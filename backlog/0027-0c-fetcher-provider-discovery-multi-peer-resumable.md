---
id: "0027"
title: '0c: Fetcher: provider discovery, multi-peer resumable blob download, tree fetch'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0026"
    - "0020"
    - "0023"
spec_refs:
    - D15. Finding peers that have a CID
    - 'D9. Peer sessions: dial peers, not addresses'
---

## Description
`fetch/`: turns "get this CID" into efficient, verified, multi-peer transfers (D15).

- Provider discovery in order: link hints → name context (later) → connected + LAN peers via `/nu/have/1` → Amino providers → delegated routing.
- **Multi-peer blob download:** parallel ranges across peers, an endgame phase, dropping peers that send bad data (detected per 16 KiB group), and serving completed ranges onward immediately; resumable across restarts (partial blobs in the store).
- Tree fetch: `/nu/tree/1` when a peer supports it; otherwise a layer-1-only walk (one round trip per level). Blobs referenced by a tree are fetched in parallel from multiple peers.
- Session-aware: requests move to the best connection as paths change (D9).

**Acceptance**
- 10 GB from 3 peers, with one peer injecting corrupt data: completes, the corrupt peer is dropped, and throughput is near the sum of honest peers.
- Killing the process mid-download and restarting resumes without re-fetching verified ranges.

## Acceptance criteria

## Work log
