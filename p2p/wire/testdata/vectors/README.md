# Wire vectors

- `keys.json`: Ed25519 protobuf pair from the **Test vectors** table of
  <https://github.com/libp2p/specs/blob/master/peer-ids/peer-ids.md> (retrieved
  from the raw GitHub source). The RSA/secp256k1/ECDSA public keys and signatures are hex copies
  of `core/crypto/test_data/{0,2,3}.{pub,sig}` in go-libp2p **v0.50.0**, downloaded
  through the Go module proxy. Their signed message is `Libp2p is the _best_!`.
  Upstream license: MIT (go-libp2p); specs: MIT.
- `multiaddr.json`: the first four binary/string vectors in
  `TestStringToBytes` in go-multiaddr **v0.16.1** `multiaddr_test.go` (MIT).
  The TCP and onion vectors deliberately exercise opaque transport semantics
  outside Nu's selected transport subset.

- `peer-record.json`: deterministic record, envelope and PeerID generated with
  go-libp2p **v0.50.0** from the spec Ed25519 fixture, sequence 1 and address
  `/ip4/127.0.0.1/udp/1234/quic-v1`. Reproduce with
  `cd experiments && go run ./cmd/wirevectors`.

The fixed Ed25519 fixture is public test material, never a deployment key.
