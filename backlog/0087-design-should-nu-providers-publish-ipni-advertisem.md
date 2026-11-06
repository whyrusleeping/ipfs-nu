---
id: "0087"
title: 'Design: should Nu providers publish IPNI advertisements for bulk content discovery?'
status: done
priority: 4
created: "2026-10-26"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/data.md#D15. Content-provider discovery
---

## Description

Investigate (design first, no implementation commitment) whether Nu nodes should announce their content to IPNI (cid.contact) the way Filecoin storage providers (Curio) do, as a bulk complement to Amino provider records.

Motivation: Amino needs one provider record per key, republished; archive-shaped datasets with millions of blobs per node strain that. IPNI ingests chained, signed advertisements (entries chunked per context ID) and answers `/routing/v1/providers` for every multihash, which Curio uses at Filecoin scale.

Questions to answer:
- Does cid.contact ingest and serve BLAKE3 (0x1e) multihashes? (Probe like 0037 did for Amino.)
- Transport metadata: IPNI defines only bitswap, graphsync-filecoinv1 and `transport-ipfs-gateway-http`. Would Nu advertise its HTTPS gateway (RASL/trustless `/ipfs/<cid>?format=raw` for raw/DRISL CIDs) under the gateway code, register a Nu protocol code, or neither? Do Kubo/Helia trustless-gateway clients accept BLAKE3 raw CIDs?
- Advertisement granularity (per dataset version / tree root as context ID), signing with the node's libp2p Ed25519 key, publishing over HTTP (announce + served ad chain), removal on unpin/GC, and cost for a one-node archive.
- Privacy/modes: never from private/isolated clusters; interaction with public names and follow swarms; whether it changes the 0037 decision (native multihash Amino keys).

Deliverable: a recommendation recorded in docs/data.md D15 (adopt with a defined profile, or reject with reasons), plus probe evidence. Implementation, if adopted, is a separate task.

## Outcome

Decided: adopt IPNI publishing as an opt-in profile for publicly reachable nodes (docs/data.md D15 → IPNI publishing): Amino's CID set under native multihashes, one context per root, DRISL ads served as raw bytes, a registered Nu transport code (never bitswap/gateway), IsRm on unpin/GC, never private/isolated. Evidence in docs/validation.md: experiments/ipniprobe ran a live announce→find→remove round trip on cid.contact via a cloudflared tunnel; BLAKE3 and SHA-256 entries in DAG-CBOR ads were served within ~2 s, unknown metadata came back as records with no Protocols, IsRm took effect in ~40 s; CDN caches stale 404/200 for 20–40 min; non-public provider addrs are rejected; go-libipni's Publisher can't serve DAG-CBOR chains. Stale "delegated routing" announce claims fixed. Implementation proposed as 0095. One run's probe records (12D3KooWEhhR…) age out in 7–14 days.

Commit: docs, experiments: IPNI publishing decision (opt-in profile) with live cid.contact probe (0087)
