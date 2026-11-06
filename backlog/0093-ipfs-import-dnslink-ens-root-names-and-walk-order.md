---
id: "0093"
title: 'IPFS import: DNSLink/ENS root names and walk-order gateway read-ahead'
status: done
priority: 3
created: "2026-11-06"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/migration.md#Sources and transport
---

## Description
`nu import ipfs` should accept DNSLink names (example.org, /ipns/<domain>) and ENS names (vitalik.eth, via eth.limo DoH as in Kubo) as well as CIDs, and HTTP-gateway imports should not pay one round trip per tree level.

Acceptance:
- `nu import ipfs vitalik.eth` (and `--dataset`) resolves the ENS contenthash through DNSLink-over-DoH, imports the root, and records the name in the result and dataset version message; a resumed job keeps the resolved CID.
- `ipfs.dns_resolvers` maps suffixes to RFC 8484 endpoints (default eth → https://dns.eth.limo/dns-query).
- Gateway read-ahead expands blocks as they arrive and fetches in depth-first walk order; vitalik.eth's blog (1757 blocks, 103 MB, Pinata gateway) imports several times faster than the 155–172 s baseline.
- Spec: docs/migration.md (DNSLink and ENS names; gateway read-ahead), docs/api.md.

## Acceptance criteria

## Work log
- 2026-11-06: Done. Measured on the dev host, vitalik.eth's blog (bafybeia5eyom…, 1757 blocks, 1114 files, 103 MB, served only by Pinata's gateway; Pinata's Bitswap is WSS-only, so unreachable from QUIC-only Nu): the baseline took 155–172 s (one earlier run took 516 s, cause not reproduced), and 1288 of 1757 block asks waited a full ~100 ms gateway round trip, because children were queued only when the importer asked for their parent, so every tree level cost one RTT. Read-ahead now expands blocks on arrival and fetches the earliest blocks in depth-first walk order (heap keyed by link-index path), 16 at once, 64 blocks / 32 MiB window, dropping stale entries: 21–23 s (32 fetches at once: 15–19 s, not adopted, out of politeness to public gateways). The rest is Pinata's ~75 ms per-request server time plus a TLS handshake per request (httpsrc disables keep-alive; not changed). DNSLink/ENS: bridge.DNSLinkResolver (_dnslink then bare TXT, sorted first value, /ipns/ chains ≤ 8, RFC 8484 POST DoH, since eth.limo returns 400 to GET); `nu import ipfs --dataset vitalik-blog vitalik.eth` imports in 23.7 s, and the version message reads "imported from ipfs://… (vitalik.eth)". Test: TestDNSLinkResolve; bridge, node and cmd/nu suites pass.
