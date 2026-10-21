# Identify vector

`identify.json` is a deterministic go-libp2p **v0.50.0** identify protobuf with
public key, listen address, protocols, observed address, protocol/agent versions,
and the signed peer record from `p2p/wire/testdata/vectors`. Regenerate from the
separate experiments module: `go run ./cmd/wirevectors -identify`. It uses the
published Ed25519 test key, not any deployment identity.
