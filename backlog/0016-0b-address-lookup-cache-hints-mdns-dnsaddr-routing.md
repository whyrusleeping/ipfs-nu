---
id: "0016"
title: '0b: Address lookup: dnsaddr + routing/v1 peers server and client'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-10-20"
depends_on:
    - "0010"
    - "0002"
    - "0060"
spec_refs:
    - docs/networking.md#D13. Peer-address discovery
---

## Description
`p2p/lookup` (D13): finding a peer's addresses by PeerId, all sources in parallel.

- Cache, hints and mDNS are in 0060; this task adds public lookup sources to that interface.
- **dnsaddr lookup server** (part of `nu relay`'s optional services): accepts signed libp2p peer records, verifies them, and serves `_dnsaddr.<peerid-base36>.peers.nu.ipfs.io` TXT records (miekg/dns). Also serves **`GET /routing/v1/peers/{peerid}`** (IPIP-417).
- Client: resolve `/dnsaddr/...` and `routing/v1`; publish our own peer record on address changes (relay-only mode configurable).
- The Amino DHT FIND_PEER source is added later by the kad task.

**Acceptance**
- A standard go-libp2p node can dial a Nu node via `/dnsaddr/<peerid>.peers.<test-domain>/p2p/<peerid>`.
- (mDNS LAN discovery acceptance moved to 0060.)
- Fuzz the TXT/peer-record parsing.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/http (ffc9410; merged 3594b9d). p2p/lookup: DNS dnsaddr Source (recursion, limits, TCP fallback), HTTP routing/v1 peers Source via httpsrc, Directory (signed-record PUT publication, DNS + HTTP handlers) and Publisher; relay.Config.Lookup hook. Evidence: experiments/TestDNSADDRLookupInterop (stock go-libp2p dials Nu given only /dnsaddr/<id>.peers.example.test/p2p/<id>), public_test.go suite, parser fuzz seeds. Deferred: CLI/daemon wiring of public sources and the relay lookup server flags, persisted publication sequences, public TLS/authoritative DNS deployment (0039).
