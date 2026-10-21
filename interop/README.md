# Opt-in libp2p interoperability (0011)

This is a **separate Go module**, not a daemon feature. Root `go test ./...` does
not build or run it. Nu uses its own host/protocol packages over stock quic-go;
no go-libp2p dependency is added to Nu (or to this driver). Reference implementations
are built in independent Docker images from pinned upstream sources.

**Phase 0a is NOT closed:** go and rust pass both directions, Kubo and Lighthouse
pass locally, but the pinned unified-testing js-v3.x test plan has QUIC explicitly
commented out. Its manifest advertises only tcp/ws/wss. We report both JS directions
as `unsupported` and return failure, not a green empty matrix or a WebTransport
substitution. The JS CI job intentionally remains red until a supported upstream
QUIC test plan can be pinned. No acceptance-criteria change is assumed.

## Run

Requirements: Linux Docker daemon, Python 3.10+, git, outbound access to GitHub,
Docker Hub and language package registries for builds. No sudo, yq, Compose or
Python packages. Rust's first build takes several minutes and a few GB of disk.
All paths below are relative to the Nu repository root:

```sh
python3 interop/run.py transport --impl go --out interop/results/go
python3 interop/run.py transport --impl rust --out interop/results/rust
python3 interop/run.py transport --impl js --out interop/results/js  # unsupported; exit 1
# --impl all is the default and runs every pair, even after a failure.
python3 interop/run.py blackbox --out interop/results/blackbox
# Explicit consent is required; not included in the other suites:
python3 interop/run.py amino --public --out interop/results/amino
```

The runner records `results.json`, raw peer/probe logs, build logs, source pins,
Nu HEAD and dirty state. Each pair must have a successful dialer exit **and**
positive finite latency measurements with ms units. Unsupported pairs and setup
failures are nonzero exits. Use a fresh `--out` directory for each independent
run if old logs should not accumulate. Containers, anonymous volumes and uniquely
named internal networks are removed on completion/failure. Build images and
`interop/.cache` remain for fast reruns; clear the source cache when updating pins.
No other Docker resources are pruned.

The workflow runs all suites nightly, and container suites on workflow dispatch.
Dispatch has a separate `public_amino` consent switch. Every job uploads artifacts
on success **or failure**. The public job has no retries/quorum rule that could
hide a failed peer. Network outages are failures, not evidence of a Nu wire bug.
The workflow itself has not yet run on GitHub; the same commands ran locally.

## Upstream contract and pins

Contract inspected at unified-testing
[`4ade567bfea81d291278a272825a7d35e73b40dd`](https://github.com/libp2p/unified-testing/tree/4ade567bfea81d291278a272825a7d35e73b40dd/transport):

- [`transport/README.md`](https://github.com/libp2p/unified-testing/blob/4ade567bfea81d291278a272825a7d35e73b40dd/transport/README.md)
- [`images.yaml`](https://github.com/libp2p/unified-testing/blob/4ade567bfea81d291278a272825a7d35e73b40dd/transport/images.yaml)
- [`run-single-test.sh`](https://github.com/libp2p/unified-testing/blob/4ade567bfea81d291278a272825a7d35e73b40dd/transport/lib/run-single-test.sh)
- [JS transport switch (QUIC commented out)](https://github.com/libp2p/unified-testing/blob/4ade567bfea81d291278a272825a7d35e73b40dd/transport/images/js/v3.x/test/fixtures/get-libp2p.ts)

`run.py` is a small runner for **the actual upstream transport test binaries**, not
a reimplementation of their ping tests or a run of upstream's entire bash matrix.
It builds the go-v0.47-pre and rust-v0.56 source revisions selected by that manifest,
applies that same revision's upstream `transport-fix.patch` files, and runs the
Nu-only QUIC submatrix on Docker networks with Redis coordination. Dockerfiles use
fixed Go 1.26.0/Rust 1.90.0 build tools rather than upstream's old/moving tool images;
Go uses `-mod=readonly`, Rust uses `--locked`. See `SOURCES` in `run.py` for exact
commits. Black-box image manifests and Redis are digest-pinned there as well.

Nu's test-plan image is independently reusable:

```sh
docker build -f interop/Dockerfile -t nu-interop:local .
```

Default entrypoint is `nu-interop -mode transport`:

| Modern environment | Legacy alias | Meaning |
|---|---|---|
| `TRANSPORT=quic-v1` | `transport` | Only supported transport |
| `IS_DIALER=true/false` | `is_dialer` | Role |
| `LISTENER_IP` | `ip` | Bind IP; default `0.0.0.0` |
| `REDIS_ADDR` | `redis_addr` | Host:port; default `redis:6379` |
| `TEST_TIMEOUT_SECS` | `test_timeout_seconds` | Overall deadline, default 180 |
| `TEST_KEY` | none | Required with modern `TRANSPORT` |

Modern Redis list key is `${TEST_KEY}_listener_multiaddr`; legacy key is
`listenerAddr`. Listener RPUSHes its reachable literal QUIC-v1 multiaddr including
PeerId, then serves until the runner stops it. Dialer BLPOPs, authenticates/dials,
pings and exits. Redis must be ready before starting the test; the runner explicitly
checks readiness. Coordination uses `redis-cli`, supplied in the image, not a
product dependency. QUIC supplies TLS/muxing; do **not** set `SECURE_CHANNEL` or
`MUXER` for the upstream Rust test plan (even empty values are rejected).

Successful dialers emit the original single-line JSON keys
`handshakePlusOneRTTMillis` and `pingRTTMilllis` (upstream's triple-l spelling).
Modern mode additionally emits upstream's `latency` YAML with
`handshake_plus_one_rtt`, `ping_rtt`, `unit: ms`. Diagnostic output goes to stderr.
The small runner accepts upstream Go's four-space YAML indentation and Rust/Nu's
two spaces; upstream's current shell extractor only recognizes two spaces.

## Black-box driver

```sh
(cd interop && go build -o nu-interop .)
interop/nu-interop -addr /ip4/IP/udp/PORT/quic-v1/p2p/PEER
interop/nu-interop -addr /ip4/IP/udp/PORT/quic-v1/p2p/PEER -ping eth2
interop/nu-interop -addr /ip4/IP/udp/PORT/quic-v1/p2p/PEER -ping none
```

`probe` mode defaults to libp2p ping. It emits one JSON record, including partial
success/error on failure, authenticated key type, agent and identify protocol list.
It never replaces a requested ping with another protocol. Key types use the libp2p
protobuf numbers: RSA=0, Ed25519=1, secp256k1=2, ECDSA=3. Probe total time includes
identify; only transport mode's handshake-plus-one-RTT field is the suite metric.

- **Kubo v0.40.0:** disposable repo, only QUIC listener, empty bootstrap list,
  routing disabled, internal Docker network. Connect, identify, `/ipfs/ping/1.0.0`.
- **Lighthouse v8.0.1:** disposable genesis-start beacon node on an internal Docker
  network, no real execution engine, no validators, no public network access.
  The absent execution endpoint produces expected warnings. Connect and identify
  authenticate its **secp256k1** identity (asserted, not inferred from the name).
  Lighthouse does not advertise `/ipfs/ping/1.0.0`; it supports Ethereum's
  `/eth2/beacon_chain/req/ping/1/ssz_snappy`. The driver sends a uint64 metadata
  sequence, validates the success/8-byte header and reads a bounded snappy Pong.
  This is an Ethereum-native ping, **not** a claim that Lighthouse implements IPFS
  ping or that Nu participates in Ethereum consensus. Snappy stays in this module.
- **Amino:** `amino.json` pins three PeerIds plus DNS host/UDP port. These endpoints
  were obtained from `_dnsaddr.bootstrap.libp2p.io` and its am6/sv15/ny5 TXT records
  on 2026-10-21. Only A resolution may change: identity is always checked against
  the pinned PeerId. One bounded connect/identify per peer, no outgoing ping,
  DHT queries, content transfer or daemon discovery. Results log the resolved IP.

Results and limitations are indexed in [docs/validation.md](../docs/validation.md#libp2p-container-interop-2026-10-21).
