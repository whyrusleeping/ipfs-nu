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
    - "0012"
    - "0037"
spec_refs:
    - docs/data.md#D15. Content-provider discovery
---

## Description
`kad/`: Amino-compatible Kademlia (`/ipfs/kad/1.0.0`).

- Client: routing table, iterative FIND_NODE / GET_PROVIDERS / ADD_PROVIDER / FIND_PEER, with bounded parallelism; plugs into address lookup as a FIND_PEER source.
- Server mode when AutoNAT v2 confirms public reachability, with conservative per-peer/global limits and measured resource budgets.
- Providing: blob, file-node and tree-root CIDs of pinned content (never interior shards), with batched/"sweep"-style reproviding by keyspace region.
- Apply the standalone Q2/citizenship decision from 0037 and record integration evidence in docs/validation.md. If deployed Amino cannot support the keys/profile responsibly, use the selected explicit routing fallback. Hint/LAN/private-member fetches do not depend on this implementation.

**Acceptance**
- Interop: our provider records are found by Kubo and vice versa (for BLAKE3 keys if Q2 passes).
- FIND_PEER resolves public Kubo nodes.
- Fuzz the kad message decoders.
- The Q2 result is documented.

## Acceptance criteria

## Work log
