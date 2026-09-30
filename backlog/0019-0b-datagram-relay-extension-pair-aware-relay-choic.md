---
id: "0019"
title: '0b+: Datagram relay extension, pair-aware relay choice, peer relays'
status: todo
priority: 3
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0013"
    - "0015"
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
    - docs/networking.md#D10. Home relays
---

## Description
Make the fallback as good as possible (D11c step 5).

- **Datagram relaying:** a Nu-only extension on Nu relays that forwards inner QUIC packets as QUIC datagrams (RFC 9221), TURN-like, keyed by peer, instead of bytes on a reliable circuit stream. Negotiated only between Nu clients and Nu relays; standard circuit v2 streams remain the fallback and all other peers ever see.
- **Pair-aware relay choice:** choose the relay minimising RTT(A→relay) + RTT(relay→B), using both peers' relay latency tables (exchanged in `/nu/path/1`).
- **Peer relays:** any publicly reachable Nu node (seed) can offer relaying for its owner's devices or an allow-list; it's advertised in its peer record / `nu.node`.

**Acceptance**
- NAT lab with 2 % packet loss: datagram relay throughput and tail latency beat stream relaying by a measured margin (report the numbers).
- A seed on the same "LAN region" is chosen over a distant project relay.

## Acceptance criteria

## Work log
