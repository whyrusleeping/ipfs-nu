---
id: "0023"
title: '0c: Kademlia (Amino) client/server + providing; verify BLAKE3 keys on Amino (Q2)'
status: done
priority: 2
created: "2026-09-24"
updated: "2026-10-22"
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
- 2026-10-21: From 0037: use native multihash keys on Amino (BLAKE3 provider records work against deployed servers). Still to verify here with Nu's own client: deterministic follow-swarm keys, provider TTL/republish, record replica fraction; better bootstrapping than the spike's single-peer start.
- 2026-10-21: Implementation merged (4e7588d, e6b73bc; 61fafde): kad/ codec, routing table, /ipfs/kad/1.0.0 client + limited server, iterative FIND_NODE/GET_PROVIDERS, provider store with TTL + 22 h republish, dnsaddr bootstrap to QUIC-v1 addrs, p2p/lookup Source + fetch.Kademlia adapters, private mode rejected. Evidence so far: 3-node in-process provide/find + FIND_PEER, experiments/kad interop with go-libp2p-kad-dht v0.50 both directions (Nu BLAKE3 provide → go finds; go SHA-256 provide → Nu finds), fuzz seeds. Open: Kubo find both ways, FIND_PEER of public Kubo nodes, live Amino run (failed on bootstrap/resets — debugging). Node wiring (AutoNAT mode, bootstrap lifecycle, pin enumeration, follow announcements) remains.
- 2026-10-21: Done (faa684d, e844f2f; merged 0995a93). Fixed multi-message stream handling and generic FIND_NODE keys found via Kubo. Evidence: NU_DOCKER=1 TestDockerKuboAminoProviderInterop (Kubo v0.40.0 findprovs returns Nu's BLAKE3 provider; Nu GET_PROVIDERS finds Kubo's), NU_AMINO=1 TestLiveNuAminoProviderKeys (SHA-256, random BLAKE3 and the deterministic follow-swarm key all provided + found by independent Nu clients, 3 records each), TestLiveNuAminoFindPeerPublicBootstrap (FIND_PEER resolves a public Kubo bootstrapper with a QUIC-v1 addr), fuzz seeds; Q2 documented. Not wired into the daemon yet (AutoNAT mode selection, bootstrap lifecycle, pinned-CID enumeration, periodic republish) → task 0072.
- 2026-10-22: Follow-up fix (e876156): responses read up to 1 MiB and truncated to 64 providers / 20 closer peers / 32 addrs per peer instead of rejected (requests stay strict at 64 KiB); popular CIDs (empty dir) now resolve on live Amino. TestOversizedResponseTruncated, FuzzDecode updated.
