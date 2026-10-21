---
id: "0012"
title: '0b: AutoNAT v2 client + server'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0010"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
    - docs/networking.md#D10. Home relays
---

## Description
AutoNAT v2 (`/libp2p/autonat/2/dial-request`, `/dial-back`), client and server.

- Client: confirm the reachability of each candidate address (listen, observed, port-mapped), prioritised; the confirmed public addresses go to identify and peer records.
- Server: rate-limited, with dial-back amplification protection per the spec; enabled on public nodes and relays.

**Acceptance**
- Interop with go-libp2p's AutoNAT v2 client and server.
- NAT lab: a public node is classified reachable and a NAT'd node unreachable within seconds of start.

## Acceptance criteria

## Work log
- 2026-10-21: Implemented on track/net (cb922b2, merged c0d41e8): p2p/proto/autonat client+server, amplification protection, nonce checks, rate limits, private-mode refusal. Evidence: loopback tests (direct, cross-IP paid, filtered UDP → Unreachable), fuzz seeds, experiments/TestAutoNATv2 vs go-libp2p v0.50.0 both directions. Remaining: NAT-lab classification acceptance (needs 0003); publishing confirmed addresses into identify/peer records and scheduling (daemon integration).
- 2026-10-21: NAT-lab acceptance met (d73e599): public node Reachable at ~0.40 s, NAT'd node Unreachable at ~0.72 s from scenario start, rootless. Publishing confirmed addresses into identify/peer records and periodic scheduling belong with daemon integration (0020).
