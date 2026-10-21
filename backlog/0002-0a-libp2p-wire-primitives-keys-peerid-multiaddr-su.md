---
id: "0002"
title: '0a: libp2p wire primitives: keys, PeerId, multiaddr subset, signed peer records'
status: in_progress
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0001"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
The encoding layer of the libp2p subset (`p2p/wire`). No networking yet.

- Keys: ed25519 (our own identity: generate, load and store the node key as a 0600 file per D17). *Verify-only* support for secp256k1 (Ethereum), ECDSA and RSA peer keys. Protobuf key encoding per the libp2p peer-id spec.
- PeerId: identity multihash / sha256 rules; string forms base58btc and CIDv1 `libp2p-key` in base32/base36 (base36 is needed for DNS labels, D13).
- Multiaddr subset: `/ip4`, `/ip6`, `/dns`, `/dns4`, `/dns6`, `/dnsaddr`, `/udp`, `/quic-v1`, `/p2p`, `/p2p-circuit`. Other protocols parse opaquely and round-trip.
- Signed envelopes + peer records (libp2p RFC 0002/0003).
- Hand-rolled or minimal protobuf for the messages we need.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Round-trip and spec test vectors (from the libp2p specs / go-libp2p test data) pass.
- Fuzz targets with seed corpora for decoders of network input (key protobuf, multiaddr bytes, signed envelope); seeds run in `go test`.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/net (afd806c, merged 4ae1cad). p2p/wire keys/PeerId/multiaddr/envelopes/peer records. Evidence: TestKeyVectors, TestNodeKeyFile, TestPeerIDVectors, TestMultiaddrVectors, TestSignedPeerRecord (byte-exact upstream vectors); fuzz seeds FuzzKeyProtobuf, FuzzMultiaddrBytes, FuzzSignedEnvelope, FuzzPeerID, FuzzProtobuf. D18 now lists decred secp256k1 (verify-only). Vector generator: experiments/cmd/wirevectors.
