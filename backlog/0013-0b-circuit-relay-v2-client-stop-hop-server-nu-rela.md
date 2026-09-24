---
id: "0013"
title: '0b: Circuit relay v2 (client, STOP, hop server `nu relay` with full-service admission)'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0010"
spec_refs:
    - D10. Home relays that always work
---

## Description
Circuit relay v2 (`/libp2p/circuit/relay/0.2.0/hop` + `/stop`) and the `nu relay` server.

- Client: reservations (with vouchers), renewal before TTL, connect-through-relay, and accepting STOP.
- Hop server (`nu relay`): QUIC/UDP 443 in v1. Standard limited circuits for everyone; **full-service circuits** (no duration/data cap) for admitted peers, where admission for now = a config allow-list (the policy is deferred, Q10).
- Relays also run identify and an AutoNAT v2 server, and answer from an alternate IP/port on request (used by NAT profiling).
- Relayed connections to *standard* libp2p peers (Noise + yamux) are **not** in scope here (Phase 3); this task provides the circuit streams that `/nu/qpath/1` runs over.

**Acceptance**
- Interop: a go-libp2p client reserves on `nu relay`; a Nu client reserves on a go-libp2p relay.
- Limits are enforced for non-admitted peers.
- Fuzz the relay message decoders.

## Acceptance criteria

## Work log
