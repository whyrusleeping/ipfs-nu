---
id: "0005"
title: '0a: QUIC-v1 host with libp2p TLS handshake on stock quic-go'
status: done
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0002"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
`p2p/host` foundations: the QUIC transport with the libp2p TLS handshake.

- One `quic.Transport` on one UDP socket per address family. Stock quic-go.
- libp2p TLS: self-signed cert with the libp2p public-key extension (OID 1.3.6.1.4.1.53594.1.1) signed by the node key; `VerifyPeerCertificate` checks the extension and derives the remote PeerId; ALPN `libp2p`. Supports verifying ed25519, secp256k1, ECDSA and RSA peer keys.
- Dial by multiaddr (`/ip{4,6}/…/udp/…/quic-v1/p2p/<id>`), accept, reject on PeerId mismatch.
- Connection manager basics: one connection per peer preferred, explicit limits per peer / per connection / global (no resource-manager model).
- Keep the socket abstraction pluggable, because magicsock (task "magicsock") will wrap it.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Two Nu nodes connect and authenticate each other; a wrong PeerId is rejected.
- One smoke test in the `experiments/` module: handshake + stream with a go-libp2p node over QUIC-v1. (rust/js interop: 0011.)
- Fuzz target with seeds for the certificate extension parser.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/net (e3dc457, merged 4ae1cad). Stock quic-go v0.64.1, libp2p TLS. Evidence: TestDirectAuthentication (mutual auth, wrong PeerId rejected), TestTLSVectors (all four spec cert vectors), FuzzTLSKeyExtension seeds, experiments/TestQUICHandshakeStream vs go-libp2p v0.50.0.
