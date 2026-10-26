---
id: "0086"
title: IPFS import over HTTP trustless gateways found through delegated routing (Filecoin/fil.pub content)
status: done
priority: 2
created: "2026-10-26"
updated: "2026-10-26"
depends_on: []
spec_refs:
    - docs/migration.md#Import contract
    - docs/data.md#D15. Content-provider discovery
---

## Description
Filecoin storage providers (Curio, used by Filecoin Onchain Cloud and fil.pub) serve IPFS content only as HTTP trustless gateways, announced to IPNI with `transport-ipfs-gateway-http`; they do not speak Bitswap, so `nu import ipfs <cid>` finds no providers today.

Scope:
- `bridge` gains a block source that asks a delegated-routing endpoint (`GET <url>/routing/v1/providers/<cid>`, default https://cid.contact) for `transport-ipfs-gateway-http` providers, fetches `<gateway>/ipfs/<cid>?format=raw` through `httpsrc` (SSRF policy, 2 MiB block limit as for Bitswap) and verifies each block by its original CID; gateways that served earlier blocks are tried first.
- Network imports (single root and pinset without CARs) race Bitswap/Amino and the gateway source per block; first verified block wins. Isolated/private cluster members do not use the public routing endpoint (D15).
- UnixFS Data messages with unknown fields (fil.pub's signature field 1001) import; each dropped field is reported as a transformation instead of failing the root.
- Spec: migration.md and api.md describe the gateway transport, config key and limits.

Acceptance:
- Offline test: a local routing endpoint + gateway (httptest) serve a UnixFS DAG; import through the gateway source succeeds and a gateway serving wrong bytes is rejected.
- Test: a UnixFS node with an unknown field imports and reports the dropped field.
- Live check (manual, recorded in the work log): `nu import ipfs <fil.pub CID>` with no CAR succeeds.

## Acceptance criteria

## Work log
- 2026-10-26: Done (024e083 on track/gw). Evidence per clause: offline — bridge.TestGatewayImport (httptest routing/v1 + liar and honest gateways: liar's bytes rejected and demoted, root equals the in-memory import, one routing lookup for the whole DAG; root carries UnixFS field 1001 → transformation `unknown UnixFS field 1001 dropped`); node.TestIPFSNetworkDryRun now runs with the gateway raced against Bitswap (`source: bitswap+gateway`). Live: `nu import ipfs bafybeigig5q…l3jy` (fil.pub hud-anim, 32 files, 2.2 MB) with no CAR → same Nu root as the CAR import (bafyreifzp6sg…p5i4) in 5.6 s; fil.pub doom-wasm (35 files, 58 MB, 261 blocks) in 29 s (sequential per-block fetching took 252 s; one CAR stream from the fastest provider takes 36 s). Providers came from cid.contact. Not covered: Nu-native content stored as Filecoin pieces (`/piece` ranges), which needs an object→piece-offset index and piece-holder discovery.
