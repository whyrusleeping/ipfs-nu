---
id: "0087"
title: 'Design: should Nu providers publish IPNI advertisements for bulk content discovery?'
status: todo
priority: 4
created: "2026-10-26"
updated: "2026-10-26"
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

## Acceptance criteria

## Work log
