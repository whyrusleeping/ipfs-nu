---
id: "0012"
title: '0b: AutoNAT v2 client + server'
status: in_progress
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
