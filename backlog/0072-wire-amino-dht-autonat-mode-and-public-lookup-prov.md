---
id: "0072"
title: Wire Amino DHT, AutoNAT mode and public lookup/providing into the daemon
status: done
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on:
    - "0023"
    - "0012"
spec_refs:
    - docs/data.md#D15. Content-provider discovery
    - docs/networking.md#D13. Peer-address discovery
---

## Description
0023 delivered `kad/` (Amino client + limited server, providers, fetch.Kademlia and lookup Source adapters) and 0016 delivered dnsaddr/routing v1 sources and a signed-record Publisher, but the daemon starts none of them. Wire them into `node/swarm` for public-mode nodes only (never private/isolated clusters): Amino client by default, server mode only when AutoNAT v2 reports public reachability; bootstrap lifecycle; providing per D15 (pinned blob, file-node and tree-root CIDs, never interior shards; follow pins: swarm key + current root) with republish; `--no-announce` respected; kad + dnsaddr + routing v1 as lookup/provider sources behind the existing interfaces; config switches; `nu status`/`doctor` show DHT state.

Acceptance: nutest (or a small in-process DHT) shows a daemon's pinned CID found by another daemon with no link hints; `--no-announce` content is not provided; private/isolated nodes open no DHT streams; one opt-in live check (`NU_AMINO=1`) that `nu add` → another fresh node `nu get <cid>` with no hints finds it via Amino.

## Acceptance criteria

## Work log
- 2026-10-22: Coordinator notes for the implementer (from 0069): add swarm.dht (and any routing endpoint keys) to node/daemon checkIsolated; never provide cluster dataset roots (heads, versions, collection catalogs) — providing enumeration must exclude cluster/cluster-surplus/collection-*/floor/version pin sources; test it.
- 2026-10-22: Implemented (c3cac15..fe17ed7; merged 4101467). node/swarm/dht.go: swarm.dht auto|client|off, kad lifecycle, AutoNAT-driven server mode, kad + dnsaddr + routing-v1 lookup sources, fetch.Multi providers, D15 provide loop with republish, --no-announce persisted, status/doctor DHT lines. Evidence: nutest TestDHTProvideAndFindWithoutHints (4-node loopback DHT; hint-less get; no-announce not provided), TestPrivateModeRunsNoDHT, NU_AMINO live: fresh daemon B found A's CID via public Amino in 4.4 s and fetched identical bytes. In progress: exclude cluster dataset pins from providing, checkIsolated keys, CGNAT 100.64/10 not published.
- 2026-10-22: Done (6585e78, 9970967, 383042e; merged 18c95e4): provide only public-intent pins (explicit/public/my-names/follow; never cluster/collection/import-dataset pins; TestProvideEnumerationSkipsClusterPins), isolated members refuse swarm.dht/lookup_domain/routing_url, CGNAT 100.64/10 excluded from published addresses/provider records/AutoNAT candidates while explicit links keep it (TestPublicationExcludesCGNAT). Not done: follow swarm-key providing (no helper yet), server mode on live Amino (needs public address), routing table liveness eviction.
