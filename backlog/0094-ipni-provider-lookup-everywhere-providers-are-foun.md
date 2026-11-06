---
id: "0094"
title: IPNI provider lookup everywhere providers are found (Nu fetch and IPFS-import Bitswap)
status: done
priority: 3
created: "2026-11-06"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/data.md#D15. Content-provider discovery
    - docs/migration.md#Sources and transport
---

## Description

User direction (2026-11-06): IPNI support should be general across the system, not only the IPFS importer's gateway transport.

Today only `bridge.GatewaySource` asks `routing/v1/providers` (cid.contact), and only for `transport-ipfs-gateway-http` records. Bitswap provider discovery in the importer and Nu-native fetch provider discovery (`fetch.Kademlia`) use Amino only, so IPNI-announced peers are invisible to both.

Scope: one bounded routing/v1 providers client (JSON/NDJSON, the existing 256 KiB / 64-record / 15 s limits, through `httpsrc`) that returns peer ID, addresses and protocols, used by:
- the IPFS importer's Bitswap source: providers announcing `transport-bitswap` whose addresses include a QUIC-v1 multiaddr, merged with Amino results (Amino-only peers unchanged);
- Nu fetch, as D15 step 6 for Nu CIDs (a `fetch.Source` next to `fetch.Kademlia`), with the configured endpoint (`swarm.routing_url`/`ipfs.routing_url`, default cid.contact); never from private/isolated cluster members.

Acceptance:
- A fixture routing endpoint announcing a Bitswap peer over QUIC lets `nu import ipfs` fetch from it with the DHT off; a fixture announcing a Nu provider for a Nu CID lets `nu get` find it with no hints and no DHT.
- D15 and migration.md describe the lookup; one shared parser for gateway and peer records.

Publishing Nu content to IPNI is the separate design question in 0087.

## Outcome

One bounded routing/v1 providers client and parser (p2p/lookup.Providers/ParseProviders: JSON/NDJSON, 256 KiB, 64 records, 15 s, via httpsrc) is shared by the gateway transport (bridge.GatewayURLs), the importer's Bitswap source (bridge.RoutingFinder: transport-bitswap records with literal-IP QUIC-v1 addresses, asked concurrently with Amino, with per-peer address merging; an Amino bootstrap failure leaves the routing finder) and Nu fetch (fetch.Delegated as D15 step 6 in the swarm, at ipfs.routing_url: unset = cid.contact on non-loopback swarms, off/"" disables; public addresses only; never private/isolated, cleared by DisablePublic). Tests: TestIPFSImportRoutedBitswap (import from an IPNI-announced Bitswap peer with Amino finding nothing), TestBareCIDFromRoutedProvider (bare-CID get with no hints/mDNS/DHT; fails without the change), TestBitswapSourceMergesProviderAddrs (stale then current address); go test ./... passes. D15, migration.md, api.md, networking.md updated.

Commit: lookup, bridge, fetch, node, docs: delegated (IPNI) provider lookup for IPFS-import Bitswap and Nu fetch (0094)
