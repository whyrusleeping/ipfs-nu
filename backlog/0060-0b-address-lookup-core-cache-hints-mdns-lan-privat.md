---
id: "0060"
title: '0b: Address lookup core: cache, hints, mDNS (LAN/private)'
status: todo
priority: 1
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0002"
spec_refs:
    - docs/networking.md#D13. Peer-address discovery
---

## Description
Split from 0016 so private clusters and first sharing do not wait for public lookup services. `p2p/lookup` core (D13): address cache with timestamps/confidence, hints parsed from links and member records, libp2p mDNS discovery (on by default; cluster-ID-derived service name for clusters, disableable), all sources queried in parallel behind one lookup interface that 0016 later extends with dnsaddr/routing v1 and kad adds FIND_PEER to.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Two Nu nodes on the same LAN find each other via mDNS (manual check on a real LAN plus a loopback-multicast test where the OS allows it).
- Cache/hint addresses survive restart with confidence decay; cluster mDNS uses the cluster-derived name and can be disabled.
- Fuzz target with seeds for hint parsing.

## Acceptance criteria

## Work log
