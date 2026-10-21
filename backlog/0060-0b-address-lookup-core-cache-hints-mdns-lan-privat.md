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

Acceptance: two Nu nodes on an isolated LAN (NAT lab, no uplink) find each other via mDNS; cache/hint addresses survive restart with confidence decay; cluster mDNS uses the cluster-derived name and can be disabled; fuzz hint parsing. (This carries 0016's mDNS acceptance criterion.)

## Acceptance criteria

## Work log
