---
id: "0018"
title: '0b+: NAT profiling (mapping, port allocation, filtering, lifetime, hairpin, IPv6)'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0012"
    - "0013"
    - "0014"
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
Continuous "netcheck" (D11c step 1). Produces a compact **NAT profile** that is exchanged with peers and shown in `nu doctor`.

- Mapping behaviour from ≥ 3 observers on different IPs and ports (relays + public peers): EIM / address-dependent / address+port-dependent.
- Port allocation pattern for EDM: preserving / sequential (δ, jitter via rapid back-to-back probes) / random.
- Filtering behaviour, via relays replying from an alternate IP/port (RFC 5780-style).
- Mapping lifetime (idle-then-probe); hairpinning; IPv6 availability and firewall; UDP blocked; port mapping available.
- Re-run on start, on every network-change event, and periodically. Keep it cheap (few packets).

**Acceptance**
- The NAT lab classifies every configured NAT behaviour correctly (mapping, allocation, filtering, timeout within ±20 %) on both IPv4 and double NAT.
- Profile encoding is stable and versioned.

## Acceptance criteria

## Work log
