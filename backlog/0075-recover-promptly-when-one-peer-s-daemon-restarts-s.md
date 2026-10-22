---
id: "0075"
title: Recover promptly when one peer's daemon restarts (stale inner QUIC session on the survivor)
status: done
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/networking.md#D9. Peer sessions
    - docs/networking.md#D12. Network changes
---

## Description
Found in 0041 (and related to 0029 scenario 3): when ONE side's daemon restarts (same identity, new process/sockets), the surviving peer keeps its old inner QUIC session over magicsock/qpath, and an immediate `nu get` retry fails until that session expires (idle timeout). The 0041/0029 resume fixtures currently restart BOTH daemons to pass.

Expected: the survivor notices promptly and re-establishes. Candidates: QUIC stateless reset handling on the virtual path (does the restarted peer send stateless resets for unknown CIDs over qpath/direct, and does magicsock/quic-go accept them?), detecting a new remote identify/peer-record sequence or relay STOP from the same PeerId as a restart signal and closing the stale session, session-level liveness (ping on request failure) before reusing a connection, and fetch retry with session re-race.

Acceptance: real-daemon natlab tests (direct, relayed and WSS-relayed): restart only the provider daemon mid-`get` → the same get completes without caller error within a bounded time (record it); restart only the recipient → a new `get` resumes from the durable verified partial immediately; documented in docs/networking.md D9/D12.

## Acceptance criteria

## Work log
- 2026-10-22: Done (951088c; merged 45ac419). Root causes: QUIC stateless-reset key regenerated per start (now derived, domain-separated, from the node identity so restarted peers' resets are accepted); duplicate same-direction reconnects rejected while a stale connection held the slot (now liveness-probed and retired); only the lower PeerID repaired dead relay legs (both now, with stagger); one reset counted as several failures (errors carry connection generation). Evidence (real daemons, rootless natlab, direct / QUIC relay / WSS relay): provider-only SIGKILL mid-get → same get completes (0.40–1.09 s recovery); recipient-only restart → new get resumes from durable partial (0.24–1.16 s); WSS/CONNECT fixture now restarts only the recipient. Independent review requested.
