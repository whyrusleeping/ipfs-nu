---
id: "0013"
title: '0b: Circuit relay v2 (client, STOP, hop server `nu relay` with full-service admission)'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0010"
    - "0012"
spec_refs:
    - docs/networking.md#D10. Home relays
---

## Description
Circuit relay v2 (`/libp2p/circuit/relay/0.2.0/hop` + `/stop`) and the `nu relay` server.

- Client: reservations (with vouchers), renewal before TTL, connect-through-relay, and accepting STOP.
- Hop server (`nu relay`): initial QUIC/UDP 443. Standard limited circuits for non-admitted peers; admitted Nu circuits support sustained transfers. Development may use bounded allow-lists. Production admission/abuse/quotas/operators/cost are explicitly owned by 0039, not a deferred policy hidden inside zero-config. This implementation supports the selected controls without requiring account identity for content commands.
- Relays also run identify and an AutoNAT v2 server, and answer from an alternate IP/port on request (used by NAT profiling).
- Relayed connections to *standard* libp2p peers (Noise + yamux) are **not** in scope here (standard-peer extensions); production Nu HTTPS-compatible reach is separate 0041. This task provides the circuit streams that `/nu/qpath/1` runs over.

**Acceptance**
- Interop: a go-libp2p client reserves on `nu relay`; a Nu client reserves on a go-libp2p relay.
- Limits are enforced for non-admitted peers.
- Fuzz the relay message decoders.

## Acceptance criteria

## Work log
