# experiments

A separate Go module for spikes and interop smoke tests. It may depend on go-libp2p, Kubo
or other reference implementations; the main module never imports it. Run its tests from
this directory (`cd experiments && go test ./...`); they are not part of the root suite.

## quic-go CID callback (0070)

`quicgo-cid/` is a **separate nested module** (not covered by `go test ./...` here) that
builds against a locally patched quic-go created by `quicgo-cid/setup.sh`. It holds the
proposed upstream patch, submission text and the before/after Nu-path benchmark; see
[its README](quicgo-cid/README.md).

## Circuit relay v2 (0013)

`go test -run '^TestCircuitV2$' -v .` checks actual go-libp2p reservation clients
against Nu's standalone relay and Nu's client against an upstream relay, including
signed voucher verification in both directions. It also sends a raw Nu
CONNECT/STOP stream through the upstream relay. This is not evidence for stock
peers' end-to-end circuit security upgrades (outside Nu's QUIC-only scope) or Nu's qpath layer.

## Manual two-machine LAN check (0060/0040)

On each machine, run `go run ./cmd/mdns-peer -key ./test-node.key` once to print
its stable PeerId, then interrupt. Use disposable test identities for this probe;
don't reuse production keys. Run again on **both machines**, passing
the other's ID:

```
go run ./cmd/mdns-peer -key ./test-node.key -peer <other-peer-id> -duration 1m
```

Allow UDP 5353 multicast and the ephemeral QUIC listener (or set `-listen IP:PORT`
for a fixed firewall rule). Each process must print `mDNS discovered and
authenticated` and a ping RTT, without supplying any remote IP address. Optionally
use the same `-cluster <id>` on both; different cluster IDs must not discover one
another. No two-physical-machine run has been recorded by the coding agent;
loopback multicast and in-process go-libp2p discovery/session transfer on a LAN
interface are automated evidence, not a substitute for this manual check.
