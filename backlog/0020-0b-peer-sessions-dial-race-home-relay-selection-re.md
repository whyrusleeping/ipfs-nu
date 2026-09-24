---
id: "0020"
title: '0b: Peer sessions, dial race, home relay selection/renewal/failover'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0013"
    - "0016"
    - "0017"
    - "0012"
spec_refs:
    - 'D9. Peer sessions: dial peers, not addresses'
    - D10. Home relays that always work
---

## Description
`p2p/host` session layer (D9) + home relay management (D10).

- `connect(PeerId)` → session: owns all connections to a peer (a nu-path connection for Nu peers; direct QUIC for public libp2p peers); new streams go to the best path; resolves addresses via the lookup task.
- **Dial race:** all known routes concurrently with short staggers (LAN → IPv6 → public IPv4 → relay; relay immediately if no direct address has worked recently). The first success carries data.
- **Home relays:** when not publicly reachable (per AutoNAT v2), always hold a reservation on a home relay + a backup, chosen by latency from the configured list; renew early; fail over automatically; announce via identify push and the peer record.
- Plain-English error reporting (e.g. "peer offline since 14:02; last seen via relay X").

**Acceptance (NAT lab)**
- 100 % connection success across the matrix except UDP-blocked, which fails with a clear message.
- Time to first byte ≤ relay RTT + handshake.
- Killing the home relay mid-session fails over without the caller seeing an error.

## Acceptance criteria

## Work log
