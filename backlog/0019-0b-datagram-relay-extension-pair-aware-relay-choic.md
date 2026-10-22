---
id: "0019"
title: '0b+: Datagram relay extension, pair-aware relay choice, peer relays'
status: in_review
priority: 3
created: "2026-09-24"
updated: "2026-10-22"
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
- 2026-10-22: Implemented (2e8c476..783a716; merged with coordinator conflict resolution vs 0041/0024). Datagram relay extension (/nu/relay/datagram/1, 1202-byte budget probes, per-packet stream fallback), /nu/relays/1 pair-aware choice (min combined RTT, hysteresis) with re-homing that keeps the inner connection, opt-in peer relays (swarm.peer_relay_allow); also fixed a pre-existing LazySelect full-duplex deadlock (TestLazySelectFullDuplexNoDeadlock). Evidence: loopback upgrade/fallback/budget/admission tests; natlab TestDatagramRelayBottleneckNATLab (4 Mbit/s bottleneck: loaded p95 44 ms datagram vs 64–72 ms stream, equal throughput); TestDaemonPairAwarePeerRelay (real daemons re-home to the lower-RTT peer relay in ~1.7 s, inner conn kept); coordinator re-ran TestDaemonNAT*, WSS UDP-blocked and pair-aware suites on the merged tree: pass. NOT MET: acceptance 'under 2% random loss datagram beats stream' — measured worse (loaded p95 109–130 vs 86–103 ms) because recovery becomes end-to-end and DATAGRAM frames stay under the outer congestion controller. Coordinator decision: default datagram mode OFF (opt-in) until a loss-aware policy exists; follow-up assigned. Awaiting user review of the acceptance gap.
