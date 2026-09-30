---
id: "0016"
title: '0b: Address lookup: cache, hints, mDNS, dnsaddr + routing/v1 peers server and client'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0010"
    - "0002"
spec_refs:
    - docs/networking.md#D13. Peer-address discovery
---

## Description
`p2p/lookup` (D13): finding a peer's addresses by PeerId, all sources in parallel.

- Address cache with timestamps and confidence; hints parsed from links.
- libp2p **mDNS** discovery (on by default); LAN-only operation with no internet.
- **dnsaddr lookup server** (part of `nu relay`'s optional services): accepts signed libp2p peer records, verifies them, and serves `_dnsaddr.<peerid-base36>.peers.nu.ipfs.io` TXT records (miekg/dns). Also serves **`GET /routing/v1/peers/{peerid}`** (IPIP-417).
- Client: resolve `/dnsaddr/...` and `routing/v1`; publish our own peer record on address changes (relay-only mode configurable).
- The Amino DHT FIND_PEER source is added later by the kad task.

**Acceptance**
- A standard go-libp2p node can dial a Nu node via `/dnsaddr/<peerid>.peers.<test-domain>/p2p/<peerid>`.
- Two Nu nodes on an isolated LAN (NAT lab, no uplink) find each other via mDNS.
- Fuzz the TXT/peer-record parsing.

## Acceptance criteria

## Work log
