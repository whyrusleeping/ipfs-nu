---
id: "0095"
title: Opt-in IPNI publishing for publicly reachable providers (D15 profile)
status: proposed
priority: 4
created: "2026-11-06"
updated: "2026-11-06"
depends_on:
    - "0087"
spec_refs:
    - docs/data.md#D15. Content-provider discovery
---

## Description
Implement the IPNI publishing profile decided in 0087 (docs/data.md → D15 → IPNI publishing).

1. The ingest probe is done. 0087's run with `experiments/ipniprobe` showed cid.contact ingests, serves and removes BLAKE3 entries in DAG-CBOR ads (docs/validation.md, 0087 section). Repeat it against Nu's own publisher before shipping. Serve the stored block bytes; go-libipni's publisher re-encodes them as dag-json. Use only public provider addresses.
2. Ask for a Nu transport code in the multicodec table (`/nu/bytes/1` with `/nu/have/1` metadata). This is an external step; the profile cannot ship with `transport-bitswap` or gateway metadata.
3. Opt-in config (for example `provide.ipni`), refused in private and isolated clusters and on nodes without a public QUIC address. Ads and entry chunks are DRISL blocks with SHA-256 CIDs, one context per announced root and 16384 multihashes per chunk, signed with the node's Ed25519 key. They are served at `/ipni/v1/ad/` over libp2p HTTP on the QUIC host, announced to `ipfs.routing_url`, and removed with `IsRm` on unpin, GC or `--no-announce`.
4. Measure the cost of a one-node archive (ad bytes, indexer pull time) against Amino reprovide, and decide whether IPNI-publishing nodes can drop blob keys from Amino.

Acceptance: a fresh Nu node fetches a bare CID through step 6 from an IPNI-publishing node with no hints, mDNS or DHT. A live cid.contact round trip (announce, find, remove) of Nu's own publisher is recorded. Private and isolated configurations refuse the setting.

## Acceptance criteria

## Work log
