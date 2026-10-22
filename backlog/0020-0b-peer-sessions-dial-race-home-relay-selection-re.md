---
id: "0020"
title: '0b: Peer sessions, dial race, home relay selection/renewal/failover'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-22"
depends_on:
    - "0013"
    - "0016"
    - "0017"
    - "0012"
    - "0040"
spec_refs:
    - docs/networking.md#D9. Peer sessions
    - docs/networking.md#D10. Home relays
---

## Description
Extend the minimal direct session layer (0040) with public path racing and home relay management (D9/D10). This is not a prerequisite for the direct/LAN/private-archive fetcher. Public mode alone reserves relays; private/isolated mode disables defaults.

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
- 2026-10-22: Foundation merged (dc6267d, ecd3a3b): SessionConfig.Dial transport seam (circuit hints race immediately for cold peers), identify address-provider seam with signed snapshot pushes and virtual-address filtering, p2p/homerelay manager (latency-ranked home/backup reservations, renewal, failover, Disabled mode). Remaining: node/swarm connectivity assembly, qpath admission + single recovery initiator, address provider shared with share links, disco authorization policy, netmon hooks, config/status, natlab two-daemon acceptance.
