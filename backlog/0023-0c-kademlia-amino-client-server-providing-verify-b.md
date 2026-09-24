---
id: "0023"
title: '0c: Kademlia (Amino) client/server + providing; verify BLAKE3 keys on Amino (Q2)'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0010"
    - "0004"
spec_refs:
    - D15. Finding peers that have a CID
---

## Description
`kad/`: Amino-compatible Kademlia (`/ipfs/kad/1.0.0`).

- Client: routing table, iterative FIND_NODE / GET_PROVIDERS / ADD_PROVIDER / FIND_PEER, with bounded parallelism; plugs into address lookup as a FIND_PEER source.
- Server mode when AutoNAT v2 confirms public reachability, with conservative limits (Q23).
- Providing: blob, file-node and tree-root CIDs of pinned content (never interior shards), with batched/"sweep"-style reproviding by keyspace region.
- **Q2 verification first:** confirm that Amino servers (Kubo, and other popular DHT server implementations) accept, store and return provider records for BLAKE3 (0x1e) multihash keys. Record the result in the spec. If they don't, fall back to delegated routing and/or a Nu kad protocol ID.

**Acceptance**
- Interop: our provider records are found by Kubo and vice versa (for BLAKE3 keys if Q2 passes).
- FIND_PEER resolves public Kubo nodes.
- Fuzz the kad message decoders.
- The Q2 result is documented.

## Acceptance criteria

## Work log
