# Validation and measurements

[Spec overview](../spec.md) · [Product brief](product.md) · [Operations](operations.md) · [Networking](networking.md)

**No field NAT-traversal or scale results are recorded yet.** Specification figures are
hypotheses and sizing targets, not measured capabilities. Record reproducible results here
(or link reports indexed here), including negative results. Synthetic regression tests do
not establish field success.

## Libp2p container interop (2026-10-21)

Task 0011; [runner, contract, exact source/image pins and reproduction](../interop/README.md).
**All six transport directions pass locally**, including js-libp2p with
`@chainsafe/libp2p-quic`. This supersedes the initial unsupported-JS result: the
stock unified-testing JS plan still comments out QUIC, but ChainSafe provides a
usable transport, now exercised by `interop/js/` with the same env/Redis/result
contract. No unsupported exemption or spec waiver was needed. Remote CI execution
and artifact publication remain unobserved; local evidence alone is not a claim
that the full Phase 0a exit has been declared.

Local environment: Linux 5.15.0-176-generic x86_64, Docker 29.4.1, no sudo.
Nu base `ef770d8` plus this interop update; Nu image built with Go 1.26.0.
Upstream unified-testing `4ade567bfea81d291278a272825a7d35e73b40dd`, using its
actual transport test binaries and contract patches: go-v0.47-pre
`dd26469c52a7ba555095df0f8088ae1fff8c7627`, rust-v0.56
`70082df7e6181722630eabc5de5373733aac9a21`. The bounded runner executes the
Nu-only submatrix rather than the upstream multi-thousand-pair shell harness.
The JS image pins Node 24.10.0 (Debian/glibc), js-libp2p 3.5.1, ChainSafe QUIC
2.1.5, identify 4.1.18 and ping 3.1.17, with npm integrity-locked dependencies.
ChainSafe's linux-x64-gnu native binary supports both roles; no native build is
needed. All required components built and ran locally; none were deferred for size.

| Dialer → listener | Local result | Handshake + first ping (ms) | Ping RTT (ms) |
|---|---|---:|---:|
| Nu → go-libp2p | pass | 5.773 | 0.449 |
| go-libp2p → Nu | pass | 5.917 | 0.314 |
| Nu → rust-libp2p | pass | 4.529 | 0.214 |
| rust-libp2p → Nu | pass | 4.770 | 0.349 |
| Nu → js-libp2p 3.5.1 + ChainSafe QUIC 2.1.5 | pass | 69.588 | 1.495 |
| js-libp2p 3.5.1 + ChainSafe QUIC 2.1.5 → Nu | pass | 76.752 | 0 (integer-ms clock) |

These are single smoke samples on local Docker networks, **not performance
benchmarks**. JS ping validates the echoed bytes but measures with `Date.now()`;
a valid sub-ms ping may report 0 ms (an earlier sample reported 1 ms). The runner
accepts finite nonnegative ping latency and requires a positive handshake measurement
and successful exit. Both modern uppercase env/Redis coordination and YAML latency
fields are exercised. A separate Nu↔Nu Docker smoke passed using lowercase legacy
env vars, the `listenerAddr` Redis key and the exact two-key single-line JSON
result contract. Calling the public runner without `--public` was rejected before
starting any containers.

| Black-box target | Local evidence |
|---|---|
| Kubo v0.40.0 (`kubo/0.40.0/882b7d2`) | QUIC connect, authenticated Ed25519 identity, identify, `/ipfs/ping/1.0.0`; ping 0.322 ms |
| Lighthouse v8.0.1 (`Lighthouse/v8.0.1-ced49dd/x86_64-linux`) | QUIC connect, authenticated **secp256k1** key type asserted, identify, Ethereum `/eth2/beacon_chain/req/ping/1/ssz_snappy`; ping 0.541 ms |

Lighthouse does **not** advertise IPFS ping. Its native Ping/Pong uses an SSZ
uint64 metadata sequence and snappy framing; this is not an IPFS-ping pass or
Ethereum consensus participation. Both black-box containers use disposable
state and an internal Docker network. Lighthouse starts from its bundled genesis
with a deliberately absent execution engine; expected execution warnings do not
prevent its P2P service from answering. No validator or public chain sync runs.

**Optional Lodestar v1.50.0 attempt:** the digest-pinned image starts a disposable
minimal dev genesis with no running validators, mock execution and discovery off.
QUIC connects and authenticates the remote secp256k1 key, but identify gets
`unexpected EOF`, so ping is **not** counted as tested. Lodestar's
[peer manager explicitly rejects non-secp256k1 remote identities](https://github.com/ChainSafe/lodestar/blob/003bd66543fa65b5c1e954eec9e8077054c226dc/packages/beacon-node/src/network/peers/peerManager.ts#L754-L761),
including Nu's Ed25519 key. An independent js-libp2p Ed25519 control reproduces
identify EOF and Lodestar's outgoing Goodbye. This is a client admission policy,
not evidence of a Nu QUIC bug or missing platform binary (the image includes
ChainSafe QUIC 2.1.5). The probe is retained behind `blackbox --client lodestar`
(exit 1) for reproduction, not enabled in the required/default nightly suite.
Changing Nu's own identity policy is deliberately not part of this interop patch.

Public Amino was separately opted into locally in the original `43ffedc` run:
all three pinned identities in
[`interop/amino.json`](../interop/amino.json) connected and identified, with no
outgoing ping or DHT query. The pins came from bootstrap DNS TXT records on this
date, not an unbounded peer discovery walk:

| Pinned endpoint | Resolved IPv4 / UDP | Observed agent | Result |
|---|---|---|---|
| lith-1.bootstrap.ipni.io | 46.166.169.131:4001 | kubo/0.43.1/acc53c0/bootstrap.ipni.io | pass |
| chic-1-b.bootstrap.ipni.io | 84.32.48.211:4011 | kubo/0.43.1/acc53c0/bootstrap.ipni.io | pass |
| chic-1.bootstrap.ipni.io | 84.32.48.211:4001 | kubo/0.43.1/acc53c0/bootstrap.ipni.io | pass |

Reproduce from the repository root:

```sh
python3 interop/run.py transport --impl all --out interop/results/transport
# All six rows now pass; any real test/setup failure still returns nonzero.
python3 interop/run.py blackbox --out interop/results/blackbox
python3 interop/run.py amino --public --out interop/results/amino
```

Local raw evidence remains in ignored `interop/results/chainsafe-final-matrix/`,
`chainsafe-final-blackbox/`, and the original `amino/` (results JSON, per-container
logs, identity/protocol JSON and build logs). The optional Lodestar failure/control
is recorded under `interop/results/lodestar-final/` and `lodestar-js-control/`. `.github/workflows/interop.yml` runs these commands nightly
and on demand, uploading results even on failure; public Amino on manual dispatch
requires its explicit input. **Remote CI execution/artifact publication has not
yet been observed**, because this branch has not been pushed.

No Nu `p2p/` bug was found or fixed. One harness mismatch was fixed: upstream Go
emits four-space YAML while Rust/Nu emit two-space YAML (the upstream shell
extractor only accepts two). No main-module dependencies changed. Pre-commit
`go vet ./... && staticcheck ./... && go test ./... && ./scripts/depguard.sh`
passed, as did the separate interop module's vet/staticcheck/build checks and
Python compilation. Root tests remain network/Docker independent.

Deferred/unmet: remote CI execution; Lodestar connect/identify/ping with Nu's
Ed25519 identity (blocked by Lodestar policy); no claim of standard IPFS ping on
Lighthouse, reverse black-box client dialing, NAT traversal, sustained Ethereum
sessions or data exchange. The original JS capability gap is resolved without
waiving either matrix direction.

## Linux NAT lab smoke (2026-10-21)

Reproduce with `go test -tags natlab -count=1 -v ./p2p/natlab`.
[Harness API, model limits and deferred scenarios](../p2p/natlab/README.md).
Local environment: Linux 5.15.0-176-generic, Go 1.26.2 linux/amd64, unprivileged
user with rootless user+network namespaces; no sudo and no external traffic.
All three tests passed in about 6 seconds total:

| Check | Observed evidence |
|---|---|
| AutoNAT v2 (0012) | Public candidate Reachable at 0.40s; masqueraded candidate Unreachable at 0.72s from scenario startup; candidate was the actual observer-reported NAT endpoint, not an unrouted private address |
| RFC fixture mapping smoke (0003) | One inside socket to three destinations (two IPs): EIM `5000/5000/5000`; ADM `5000/5000/5001`; random APDM `25240/16403/50461` on one public IP |
| Filtering | Same endpoint allowed in all three representatives; alternate source port allowed only in EIF/ADF; alternate IP allowed only in EIF, before contacting either alternate destination |
| Changes and blocked paths | Hairpin off timed out and on succeeded; conntrack/pinhole flush, WAN address rebinding, private interface down/up plus route restoration succeeded; all-forwarded-UDP block timed out |

Random allocations vary per run. Fixture reservations are static and bounded;
these checks do not prove a dynamic NAT allocator, precise mapping lifetimes,
file-transfer continuity, traversal success rates or the full topology/matrix.
`NU_NATLAB_RESULTS=/absolute/path.ndjson` writes machine-readable pair outcomes;
first-byte/direct durations are absent until a consumer measures them. The
on-demand `natlab` workflow uses root as the stock-runner fallback; remote CI
execution is not part of this local evidence.

## Circuit relay v2 smoke (2026-10-21)

[API, limits and exact scope](../p2p/proto/circuit/README.md).
Linux/Go environment as above; no external peers or host-root privileges used.

- `go test ./p2p/proto/circuit ./relay`: loopback client/relay/target raw echo and
  independent FIN, voucher peer binding, renewal, missing-reservation rejection,
  IP reservation and global circuit slots, full-service admitted pairs, and a
  32-byte directional cap for an unadmitted source. Default protocol tests took
  roughly 0.05s; their race-enabled run passed too. Fuzz decoder seeds and literal
  control-message vectors run in the normal package tests.
- `cd experiments && go test -run '^TestCircuitV2$' -count=1 -v .`: actual
  go-libp2p v0.50.0 reservation client on Nu and Nu client on go-libp2p relay;
  signed vouchers accepted in both directions. A Nu pair also exchanged a raw
  CONNECT/STOP stream through the upstream relay. Passed in approximately 0.02s.
- `go test -tags natlab -run '^TestCircuitToNATTarget$' -count=1 -v ./p2p/natlab`:
  real masqueraded reservation from a private target, public-client circuit echo,
  timed renewal before TTL, and an actual 1s circuit-duration reset. Passed
  rootless in approximately 2.0s and emitted a standalone scenario outcome.

These prove circuit-v2 control/byte-stream interoperability, **not** Noise/yamux
secure connections, `/nu/qpath/1`, content transfer, physical-network traversal
rates or production public-relay operating capacity. Default public operation and
operator admission/abuse quotas remain gated on 0039. AutoNAT uses a fresh source
port; alternate-source-IP profiling request semantics are not provided by the
standard AutoNAT v2 protocol and are not claimed by this implementation.

### Port mapping and reaction pipeline (0014, 2026-10-21)

Reproduce with `go test -tags natlab -run '^TestNATLab' -v ./p2p/portmap ./p2p/netmon`
(or the full `go test -tags natlab ./p2p/...`). In the same rootless environment:

- PCP MAP, NAT-PMP and UPnP SSDP/SOAP clients obtained mappings from in-test router
  servers. Each successful protocol request installed a real nft DNAT rule before its
  reply. A new public UDP source reached a private application that had never contacted
  it. Resetting the server epoch to **zero** and deleting its rule triggered re-mapping;
  fresh-source inbound probes worked again. Explicit release removed reachability for
  another fresh source, avoiding conntrack false positives. Measured protocol scenarios:
  PCP 414 ms, NAT-PMP 435 ms, UPnP 439 ms; complete isolated test 1.75 s.
- A real dummy-interface creation, link-up and address assignment delivered Linux
  rtnetlink events and completed all six registered D12 pipeline hooks in **10.5 ms**
  from the start of the scripted changes (acceptance bound: 1 s).
- Fast tests additionally cover half-life renewal (one-second granted lease), PCP nonce
  correlation, PEER/IPv6 packet handling, UPnP endpoint/nesting guards, stage order/error
  continuation, coalescing and inferred sleep/wake. Wire vectors are in
  `testdata/vectors/portmap.json`; parser fuzz seeds run in the normal test suite.
- Pure-Go Darwin arm64 cross-build passed. This is compile evidence, not macOS runtime
  route-monitor, sleep/wake or router compatibility evidence.

**Real-daemon follow-up (2026-10-21):** Ubuntu `miniupnpd-nftables` 2.2.3-1 was
downloaded with `apt-get download miniupnpd miniupnpd-nftables` and extracted with
`dpkg -x` into a temporary directory, without sudo. Its `--version` reports the nft
backend and its startup log confirms NAT-PMP/PCP and UPnP-IGD support. Run:
`NU_MINIUPNPD=/absolute/extracted/usr/sbin/miniupnpd go test -tags natlab -run '^TestNATLabMiniUPnPd$' -v ./p2p/portmap`.
The daemon ran as root **inside an unprivileged user/network namespace**, installed its
own nft rules, and all three Nu clients produced working fresh-source inbound UDP
reachability and removed it on release (PCP 335 ms, NAT-PMP 337 ms, UPnP 391 ms;
complete test about 1.5 s). No production client changes were needed. The daemon rejects
reserved WAN addresses, so this fully isolated topology uses `11.0.0.0/24`, with no
real-network uplink. Config, PID, lease and minissdpd paths are private temporary paths.
The on-demand CI workflow installs the nft build and runs this test explicitly; it no
longer merely installs an unused daemon. Without a binary, only this opt-in daemon test
skips with an explanation; the deterministic fixture scenarios still run.

Fake lease expiry is not a kernel lease scheduler: renewal timing is tested separately
at the client boundary. Real-daemon reboot/lease-expiry timing, IPv6 firewall/PEER
forwarding, UPnP IPv6FirewallControl, real suspend/resume and consumer-router field
validation remain deferred. Host/AutoNAT/relay/identify/path hooks need owner-side wiring;
the measured reaction invokes registered test hooks, not a production daemon's recovery.

## Stock-quic-go Nu-path spike (0038, 2026-10-21)

**Decision: GO for 0015 with D11's conservative adjustments; NO-GO for transparent
arbitrary-MTU path switching or offload preservation by simple socket forwarding.**
Stock quic-go can keep one inner connection and active stream across reliable relay,
direct and replacement relay paths without a fork. Hidden path changes do not give it
new per-path MTU/congestion state. This is feasibility evidence, not a shipped Nu-path.

[Disposable harness, reproduction commands and exact boundaries](../experiments/nupath/README.md).
Base revision `c729540` plus the 0038 spike; quic-go **v0.64.1**, Go **1.26.2**, Linux
**5.15.0-176-generic**, linux/amd64, AMD EPYC 9684X (192 logical CPUs visible; shared
machine, no pinned cores). No Internet traffic. Inner QUIC uses libp2p TLS/ALPN;
outer transport is the real Nu circuit-v2 implementation. `/nu/qpath/1` is negotiated
by multistream; packet framing is disposable, not a finalized protocol. A single
continuous deterministic stream is SHA-256 checked at EOF, with **no application
resume, inner redial or stream replacement**. Fixed-budget runs send at most 1200-byte
inner packets. The writer is deliberately paced at one 32 KiB record per millisecond;
these timings are not maximum throughput benchmarks.

Three non-race repetitions, `cd experiments && go test -count=3 -v ./nupath`:

| Active stream phase | Median (range), n=3 | Evidence |
|---|---|---|
| First 4 MiB over circuit stream, including inner setup | 251 ms (248–260 ms) | Initial reliable relay path works |
| Next 8 MiB direct | 443 ms (442–444 ms) | Same virtual address and inner connection |
| Next 1 MiB after changing to 60 ms RTT / every 50th packet dropped in each direction | 3.08 s (2.55–3.18 s) | Inner smoothed RTT became 60.7–61.5 ms; congestion window fell from 120–134 kB to 9.3–10.8 kB, not a clean per-path reset |
| 250 ms all-path outage, discard old circuit/outer connections, reconnect/reserve/reattach, then 4 MiB | 787 ms (732–830 ms) | Same inner stream finishes and hashes correctly; final RTT 421–477 µs, congestion window only 10.8–13.1 kB |
| Complete 17.0–17.1 MiB stream | 4.59 s (4.10–4.65 s) | No request-resume mechanism is hiding an inner disconnect |

Resources in those runs: 68.0–69.2 MB total Go allocation delta, final live heap delta
−0.03–3.93 MB (GC-sensitive, **not peak heap**), 54 additional live goroutines including
three outer hosts/relay and both inner transports. Packet-queue high water was 96–109
of 256 slots; the separate relay transmit queue caps at 64. A blocked-writer check
submitted 1024 packets in 1.2–2.4 ms, dropped 959 and never exceeded 64 queued datagrams;
close released/joined the blocked worker. An additional standalone `TestPaths` run,
compiled first and measured with `/usr/bin/time`, took 4.63 s wall, 1.21 s user CPU,
0.81 s system CPU and **34,384 KiB max RSS**. These are one-pair harness costs, not
production per-peer sizing or relay-capacity projections. The machine's UDP receive
buffer cap was 416 KiB despite quic-go requesting 7168 KiB; the warning was retained.

Important negative/boundary results (three repetitions):

- **MTU:** after DPLPMTUD reached 1441-byte packets, silently lowering the UDP-payload
  ceiling to 1200 bytes produced only 0–14,113 bytes of progress in 600 ms and 56–74
  oversize drops. Restoring the ceiling recovered the same stream. Explicit
  `InitialPacketSize=1200`, `DisablePathMTUDiscovery=true` instead transferred
  11.37–11.59 MB in that interval, with zero oversize drops. This is a synthetic
  payload blackhole, not an interface-MTU/ICMP measurement. Stock `mtu_discoverer.go`
  only raises the current size until a QUIC-visible migration resets it; no public
  reset exists for hidden path switches. Stock `Conn.AddPath`/`Path.Switch` concerns
  client-initiated migration between Transports, not a route hidden behind one
  virtual address; that alternative architecture was not exercised.
- **Outage bound:** all-packet loss expired the inner connection after 2.000–2.001 s,
  matching the test idle timeout. Product request-resume is still necessary and was
  **not** tested/implemented by this spike.
- **Outer DATAGRAM budget:** a 1200-byte outer packet budget rejected a 1200-byte
  payload (`DatagramTooLargeError`, maximum 1163 bytes); a 1400-byte budget delivered
  it on loopback. Nu extension headers consume additional payload. DATAGRAM support
  alone is insufficient to replace circuit framing; require enough budget on both
  relay legs or keep the reliable fallback.
- **OOB trap:** with an OOB-capable wrapper embedding `UDPConn`, stock quic-go/x/net
  bypassed the overridden `ReadMsgUDP` entirely (zero calls, while three writes used
  `WriteMsgUDP`). An explicit `ReadBatch` routed reads through the hook (four calls).
  Full virtual-address OOB/ECN/GSO semantics were not implemented; initial 0015 uses
  the safe unoptimized inner adapter. Delegating the fd is not an optimization proof.

Three rootless `go test -tags natlab -run '^TestNATPaths$' -count=3 -v ./nupath`
runs also passed: two separate EIM/APDF NAT fixtures, observer-activated mappings,
simultaneous probes on the inner QUIC sockets with public-source verification, and a
public QUIC circuit relay. First relayed 512 KiB took 30–35 ms; next direct 1 MiB took
62–64 ms. A real nft direct-pair drop followed by an explicit 100 ms fallback decision
and 512 KiB took 226–228 ms. Router WAN down/up, new outer QUIC connections and a new
circuit completed the same inner stream: 2,752,512 bytes in 556–594 ms excluding lab
setup. These demonstrate network isolation/NAT forwarding, not automatic detection,
consumer-router traversal rates or NAT discovery. Race runs of `TestPaths` and tagged
`TestNATPaths` passed, as did vet/staticcheck for the spike (including the natlab tag).

Remaining boundaries: the spike shares disco/inner QUIC on one UDP socket but uses
**separate inner and outer UDP sockets**. Production CID demultiplexing and physical
socket replacement remain 0015 work. Probe MAC/replay/destination authorization,
expected-peer binding on reattachment, finalized qpath framing, adaptive MTU, full
GSO/ECN/batching, capacity-collapse and long lossy reliable-relay performance remain
unproven. The bounded congestion tests do not establish fairness under arbitrary path
changes; reliable framing retains head-of-line blocking. Three local runs support
medians/ranges, not meaningful p95 estimates or field success claims.
**Netmon natlab flake follow-up:** the original test failed 4/20 isolated repetitions
without other packages running (`Stage:3`, expected `Rebind`). It waited until all `ip`
commands finished before reading a 32-entry, intentionally drop-oldest diagnostics queue;
link/IPv6/route notification bursts could evict the beginning of a reaction. Namespace
names and addresses were not shared across these runs. The test now records actual hook
executions in a single complete receipt and still enforces the 1 s bound; production
queue policy is unchanged. The full tagged `./p2p/... ./cluster/...` set passed three
uncached repetitions after this correction, with the real miniupnpd binary enabled.

### Public address lookup (0016, 2026-10-21)

Fast in-process tests in `p2p/lookup/public_test.go` publish a signed record through
bounded HTTP PUT, retrieve its addresses through both DNS and routing/v1 Sources, and
combine them through the existing Resolver. Twenty addresses force UDP truncation and
TCP DNS fallback. Other checks cover recursive TXT concatenation/suffix filtering,
cycle/query limits, cancellation before DNS egress, signature tampering, replay rejection,
relay-only signing, change-triggered publication, unknown JSON schemas/fields, NDJSON,
legacy 404 and forbidden upload redirects. TXT, DNS wire, routing JSON and signed-record
parser fuzz seeds run without external traffic. Tests explicitly opt into loopback HTTP;
production uses the shared HTTPS/SSRF policy.

Interop: `cd experiments && CGO_ENABLED=0 go test -run '^TestDNSADDRLookupInterop$' -v .`
passed in about 0.02 s (distinct `dnsaddr_lookup_httptrack_test.go`). A stock go-libp2p
v0.50.0 host received **only** `/dnsaddr/<Nu-base36-id>.peers.example.test/p2p/<Nu-id>`,
queried Nu's DNS server through its standard multiaddr resolver, connected over QUIC,
authenticated Nu's PeerId and exchanged an echo stream. Nu also verified a signed peer
record created by go-libp2p and returned its addresses through DNS. Everything used
loopback and test-local DNS; there was no real-network lookup or external operator.

Deferred: CLI/daemon owner wiring of public Sources, persisted publication sequences,
address-change publisher lifecycle, public TLS/authoritative-DNS deployment and operational
admission/rate policy. DNSSEC and persistent lookup-directory anti-replay are not claimed.
Provider/IPNS routing, IPIP-484 filters and DHT FIND_PEER remain separate tasks.

## Nu-path packet core (0015, 2026-10-21)

**Functional packet core implemented; near-raw throughput acceptance remains unmet.**
Production code is in `p2p/magicsock` and `p2p/proto/qpath`; no fork or new dependency.
[API, ownership, limits and next-owner seams](../p2p/magicsock/README.md) ·
[final framing and socket contract](networking.md#d11-nu-path).
Environment/dependencies are those of 0038 above, based on `f0c0bed` plus this work.
Unlike the spike, each private host has **one** UDP socket and QUIC dispatcher for both
outer circuit QUIC and inner virtual-peer QUIC. Ordinary direct peers remain physical.

Fast acceptance evidence:

- `go test ./p2p/magicsock ./p2p/proto/qpath`: one application stream echoes verified
  bytes across relay → direct → relay → a newly negotiated relay leg. Both endpoint
  connections retain their virtual addresses and authenticated PeerIds. The outer
  relay connection remains physical. Issued CIDs (not just handshake CIDs) are routed;
  retirement/close releases them. The inner TLS exporters match. Explicit virtual
  Session hints work but are not persisted in the verified-address cache or accepted
  as discovery hints; identify omits virtual observed addresses.
- Unit/fuzz **seeds** bound qpath/disco decoding, reject malformed/partial frames and
  unauthorized direct-source CID traffic, prevent live relay replacement by asserted
  metadata, enforce relay backpressure, and exercise logical read deadlines and
  virtual-address removal/non-reuse. A Linux GSO test splits a 3000-byte super-packet
  into 1200/1200/600-byte relay frames. Physical OOB is retained; inner ECN remains
  deliberately unavailable rather than being copied across reliable framing.
- `go test -tags natlab -run '^TestQpathNAT$' -count=1 -v ./p2p/proto/qpath` passed
  rootless in **0.68 s** (0.65 s isolated scenario). Two private EIM/APDF NAT fixtures
  and a public circuit relay: one authenticated inner stream survived relayed setup,
  direct upgrade, an actual nft direct-pair block, explicit relay fallback and a
  20 ms WAN interface down/up. Scripted simultaneous socket probes had public NAT
  sources checked; the direct phase took **2.5 ms** after explicit selection. Both
  endpoints sent direct and relay packets; zero adapter-queue drops in this run.
  Detection, endpoint authentication and selection were scripted, not 0017 disco.
- `cd experiments && go test -run '^TestQpathDeclined$' -count=1 -v ./nupath` passed
  in **0.02 s** against go-libp2p **v0.50.0**. Real-address QUIC passed through the
  wrapper, and an upstream peer reached through a real Nu circuit reservation
  answered security-protocol `/nu/qpath/1` with `na`. No virtual slot was allocated
  and no standard Noise/yamux fallback was attempted.
- Race-enabled tests passed for magicsock, qpath, host and core proto. Darwin arm64
  cross-build passed; this is not macOS runtime/offload evidence. Vet/staticcheck
  include the new tagged lab. The normal pre-commit suite and depguard passed after
  one unrelated `cluster.TestPrivateCluster` TempDir cleanup failure on the first
  full run (`directory not empty`); no cluster/node/store files were changed.

**Performance gap, not a successful ~5% result:** the opt-in
`BenchmarkProductionPaths` in `experiments/nupath` uses `slow.Skip`. Reproduce the
bounded check with `NU_SLOW=1 GOMAXPROCS=4 go test -run '^$' -bench
'^BenchmarkProductionPaths$' -benchtime=100ms -count=3 ./nupath`. Four-MiB streams,
identical fixed 1200-byte packet budgets and eight-byte local CIDs; setup is outside
measurement. On the final bounded check, raw QUIC measured **822–873 MB/s** (median
862), virtual direct **455–500 MB/s** (median 483), roughly **44% lower median
throughput**. This short shared-machine microbenchmark is not field capacity, but it
plainly does not satisfy the original near-raw target. The extra receive/copy queue
and CID-observer path need profiling/optimization; do not hide the result by comparing
to a differently configured raw transport. No long lossy/performance pass was run.

Deferred: authenticated/replay-resistant/destination-authorized disco and automatic
path selection/heartbeats (0017); public setup admission, home/backup relay lifecycle,
racing, reattachment/crossed-attempt policy (0020); physical socket replacement,
multihomed OOB/ECN behavior, performance tuning and long reliable-relay loss/capacity
measurements. Qpath setup by itself never authorizes serving content. The default
stack does not enable public Nu-path orchestration or silently claim this remaining
throughput criterion is complete.

## Early experiments

| Experiment | Decision/evidence required |
|---|---|
| Amino BLAKE3 provider and swarm keys (Q2) | Acceptance/retrieval against deployed servers, protocol citizenship with non-Bitswap providers, explicit routing fallback |
| Stock-quic-go Nu-path | [0038: constrained GO](#stock-quic-go-nu-path-spike-0038-2026-10-21); fixed 1200-byte inner budget, bounded relay adapter, safe offload baseline; production dispatcher/security and long-path performance remain 0015 work |
| File/tree format and Bao | Canonical vectors, wrapper/name-kind agreement, fanout/node caps, bao-tree parity and safe path/metadata rules |
| DASL/atproto conformance | DASL test suite and atproto interop vectors (data model, MST, crypto, firehose) in CI; byte-identical repo CAR round-trips and live `getRepo` verification against indigo/`goat`; SHA-256 vs BLAKE3 ingest/transfer costs for `--hash sha256` guidance |
| Archive update/retention | Small scattered changes, full large-file rewrites, appends and sharded indexes; actual transfer/headroom/history costs |
| Cluster readiness/release | Stale evidence, impossible copies/zones, concurrent epochs and partitions; no early deletion or false readiness |
| Public infrastructure | Named operators, admission/abuse model, availability, relay reservation/keepalive and relayed-byte cost model |
| API/browser boundary | Malicious pages/Host/Origin/DNS rebinding, active-content isolation and private gateway exposure |

Use disposable deployments and bounded experiments. Live routing/network tests require
operator-appropriate rate limits; do not turn verification into unsolicited load or scanning.

### Q2: Amino provider keys and citizenship (bounded live results; incomplete swarm run)

Harness: `experiments/amino/amino_live_test.go` (separate Go module; go-libp2p v0.50.0,
kad-dht v0.42.2). Live command:
`cd experiments && NU_AMINO=1 GOLOG_LOG_LEVEL=error go test -run '^TestLiveAminoProviderKeys$' -count=1 -v ./amino`.
Without `NU_AMINO=1` the test skips. **The first harness version was faulty:** it only called
`Bootstrap` (asynchronous) and returned/cancelled its setup context before establishing
connections. It reported zero routing peers and is discarded. The corrected harness explicitly
connects to the default fixed TCP bootstrapper mars (`/ip4/104.131.131.82/tcp/4001`), configures
that bootstrap peer, calls `Bootstrap`, and waits for a non-empty routing table before each
operation. Logs confirm the connection and table size; operation contexts remain alive for the
full `Provide`/`FindProvidersAsync` call. The host is client mode with default `/ipfs/kad/1.0.0`
protocol prefix and default transports.

Test pairs use fresh provider and query identities. `Provide(cid, true)` is followed by
`FindProvidersAsync` from the second client. The key cases are SHA2-256 control, random-data
BLAKE3, and exact follow-swarm recipe
`BLAKE3("nu.follow\\0did:plc:ewvi7nxzyoun6zhxrhs64oiz\\0amino-spike-dataset")`. On 2026-10-21,
the corrected initial run used a 45 s operation deadline and yielded:

| Key | Round | CID | Provider/query routing-table peers | Provide | Independent lookup | Outcome |
|---|---:|---|---:|---:|---:|---|
| SHA-256 control | 1 | random (not returned) | 1 / 2 | timed out at 45 s | not run | inconclusive |
| SHA-256 control | 2 | `bafkreifdjtdprv5frv7l7kvyosssf23sboxfby7gwxzmnlk35z2ajvkxjm` | 1 / 2 | 26.679 s | 18.322 s | provider found (1 record) |
| Random BLAKE3 | 1 | `bafkr4ic3dqouvmpwrqgermguvfr2o2yawqglm73bssoucwghpppp2vyopu` | 1 / 2 | 22.745 s | 22.256 s | provider found (1 record) |
| Random BLAKE3 | 2 | `bafkr4icwv24ndb3go2lnphftjkywib2fkz2vvd2k2xyhge5a2fxuffg2oa` | 1 / 1 | 22.040 s | 22.960 s | provider found (1 record) |
| Deterministic swarm BLAKE3 | 1 | `bafkr4ie4nrxp5ca6g4zhxq7faiflql3tvztv6zpwohutv6bvewhvrdmjjq` | 2 / 2 | timed out at 45 s | not run | inconclusive |
| Deterministic swarm BLAKE3 | 2 | same CID | 2 / 1 | timed out at 45 s | not run | inconclusive |

An additional rerun of the exact same swarm CID with 90 s operation deadlines timed out during
`Provide` in both rounds (90.78 s and 90.77 s); no GET was possible. A later pattern-matched
probe with the same 90 s limit also timed out for another random BLAKE3 CID
(`bafkr4ihow4rcw2eej3qsww27nqiqz5u7ku3yv6uvwfklwxiw6hy6jp5wze`, 90.80 s) and the same swarm
CID (90.79 s); neither reached GET. A separate earlier single SHA-256 probe completed, but
its verbose logs did not preserve timing, so it is not included in the table. Across recorded
attempts, random BLAKE3 succeeded twice and timed out once; the SHA control succeeded once and
timed out once; the deterministic swarm CID timed out five times (two 45 s and three 90 s).
This is positive evidence that deployed Amino provider paths accept and return some random
0x1e BLAKE3 keys; it is not evidence for the exact deterministic swarm CID. Per-run routing
tables were small (1–2 entries at operation start); this was a bounded client lookup, not a
fleet-level success or closest-peer-storage measurement. High and variable latency plus
key-dependent timeouts limit those conclusions.

`Provide`/`FindProvidersAsync` do not expose the peer set that accepted provider writes, so the
fraction of closest peers storing each record is **not measured**. A GET returned exactly one
provider for each successful key; this does not prove one write versus multiple writes or locate
record replicas. Server implementations/versions were not identified from bootstrap peers.
DHT debug output during the failed probes showed stale/referral peers with no `/ipfs/kad/1.0.0`
protocol, refused TCP security negotiation, and inaccessible loopback addresses; the lookup
continued through these referrals until its context expired. This explains the operation
failures better than a BLAKE3-specific rejection, but does not prove why each key's lookup
converges slowly. UDP receive-buffer warning remained (208 KiB available vs 7168 KiB requested),
while the successful Amino TCP path shows it did not prevent all requests.

HTTPS spot-checks to `delegated-ipfs.dev/routing/v1/providers/{cid}` returned HTTP 200 with
`{"Providers":[]}` for one unannounced BLAKE3 and one SHA-256 CID; `cid.contact` returned 404 for
the BLAKE3 URL. No BLAKE3 key was publicly announced via delegated routing. These observations
do not establish index/filter policy.

Source-level observation: upstream go-libp2p-kad-dht v0.42.2 `IpfsDHT.Provide` forwards
`cid.Hash()` (the CID's original multihash bytes) through the classic `/ipfs/kad/1.0.0` lookup/
put path; it does not second-hash the provider key or check for SHA-256 there. The successful
independent BLAKE3 GET is the operational evidence; no server-side namespace/hash validation
inspection was possible. Provider records carry peer ID plus `FilteredAddrs()`, not a Bitswap
capability declaration. Default `Provide(..., true)` uses classic closest-peer traversal;
optimistic provide is opt-in in this client. Provider sweeping is a local reprovide mechanism
and no TTL/republication behavior was measured.

**Recommendation:** use Amino directly for ordinary BLAKE3 blob CIDs; two independent fresh
clients retrieved both announced random BLAKE3 keys, with no routing-only SHA-256 derivation,
content alias or separate DHT protocol indicated. This is a bounded positive result, not a
promise for every peer/service. The exact deterministic follow-swarm key remains unverified
because its lookups timed out; keep swarm discovery as experimental/measure again rather than
claiming support or rejection. Preserve hint/LAN/private lookup independence. A NATed client
can participate without server mode, but announce only content it serves and addresses usable
by callers. Amino records do not advertise a Nu transfer protocol, so Bitswap-only peers cannot
fetch `/nu/bytes/1`. Publicly reachable server mode is the separate routing-service role.

## Pilot workloads

Recruit archive/mirror teams with smaller Internet Archive/Wikipedia-shaped workflows.
Record data volume, entry count, file-size distribution, deduplication, file/change patterns,
retention schedule, member/failure-domain topology, storage/network hardware, administrative
constraints and existing IPFS dependencies. Include an individual first-share/publisher case.
Test both sparse independently changing files and expensive monolithic rewrites.

The 10M-entry/10k-follower and PB/100-member targets in D19/D20 remain scale-track sizing
work, not initial validated operating limits. Establish supported pilot limits from evidence
and publish them before release. Promote sub-file reuse or placement changes only when
representative workloads justify the cost and complexity.

### Pilot candidates

**Decentralized Sentinel Collaborative Ground Segment** (vmx, IPFS Foundation;
[architecture](https://hackmd.io/@vmx/decentralized-collgs-202609),
[implementation](https://hackmd.io/@vmx/decentralized-collgs-implementation-202609)). This is
a Python/libtorrent prototype. Nodes discover each other by LAN multicast, and each scene is
a private BitTorrent v2 torrent. Ingest takes [Matadisco](https://matadisco.org/) records from
Jetstream, fetches each Earth Search STAC item, uses GDAL to extract a reduced true-colour
COG, and delivers it to one node over a web seed. Per-node data-managers select scenes by
bbox, sensing time and cloud cover, including 6 h/24 h/7 d rolling holdings. Rescuer nodes
with 50–100 GiB budgets take the least-replicated scenes. A shared collector answers copy,
rarity, holder and transfer queries. This is the [D22 collections](collections.md) workload.

Observed on 2026-10-20 from the prototype's public dashboard (`http://57.128.118.215`,
`/api/overview|nodes|transfers|dataset/<hash>`):

| Measure | Value |
|---|---|
| Nodes | 7 (`europe`, `maxcloud-5`, `rolling-6h`, `rolling-7d`, three `rescue*gib`), apparently containers sharing one host's disk |
| Items | 127,054 scenes, 3.49 TB stored; the 7-day rolling node held 93,508 (2.26 TB), about 13k scenes/day |
| Item shape | One COG file per item. In the 2,000 rarest: p10 0.12 MiB, p50 15.7 MiB, p90 58.9 MiB, max 78.6 MiB; 60% over 8 MiB, so BLAKE3 blobs |
| Metadata | About 1.5 KiB per item: the Matadisco record (`at://` URI, resource, preview) plus a STAC subset (id, collection, bbox, geometry, datetime, cloud cover, platform, EPSG) |
| Replication | 112,159 of 127,054 items (88%) had a single copy |

The rarest-first sample is biased, and the size percentiles are not a whole-collection
distribution. The full Sentinel-2 L2A rate the architecture targets is "tens of thousands
of datasets per day".

**Production shape (vmx, 2026-10-20):** real datasets are much larger than the demo, with
several files each. BitTorrent compatibility does not matter, and running the Go Nu daemon
is acceptable. For scale, one full Earth Search L2A item checked on 2026-10-20
(`S2B_28SBB_20261019_0_L2A`) has 19 HTTPS assets: 13 band COGs, a visual COG, an SCL mask,
metadata XML/JSON and a thumbnail, totalling 1.05 GiB. Its JP2 originals are 18 more assets
on `s3://`. The assets carry S3 ETags but no `file:checksum`. At 13k–20k items/day, that is
about 13–20 TiB/day of ingest, and a member keeping everything needs about 1.3–2 Gbit/s
sustained. The pilot must record the real product set, file counts and size distribution
before sizing claims.

Pilot success criteria:

- Reproduce the regional, cloud-cover, rolling and rescue policies as D22 rules, and the
  collector's questions through the holdings index.
- Keep the torrentizer's Matadisco/Jetstream discovery and GDAL step, but replace torrent
  building, the publisher and the web seed with pushes to the
  [collection item API](collections.md#ingest). Use uploads, `--nocopy` paths or URLs, with
  retries after producer restarts and no lost or duplicated items.
- Sustain at least 20k items/day from at least two union writers without lost or
  duplicated items, at the demo's item shape.
- With full multi-file scenes on declared hardware, measure push ingest and per-member
  fan-out throughput (Gbit/s), multi-source fetch of new items, and partial-item (`files`)
  holdings.
- After a member restart, an outage longer than its log or a late join, holdings converge
  with no missed items. The prototype's data-manager documents missed takes as a known
  limitation.
- Measure catalog bytes per item, index memory/disk, and selection re-evaluation cost.
- Run unchanged on a multi-host private-mode deployment without multicast.

## Reporting method

Each result records code/dependency versions, OS/architecture, hardware, network path and
bandwidth/RTT/loss, dataset/change shape, resource limits, repetitions, failures and uncertainty.
Latency reports include percentiles, not just one successful run. Cold name resolution's
working target is p95 <1 s in a declared regional healthy-PDS benchmark; publish external
identity/PDS time separately. Cache hits make no name-plane requests. This is not a universal
third-party service SLA.

Track setup time/steps, first-share/browser completion, exact-version retrieval, restore
success, operator interventions, time to readiness, unique storage overhead, changed-content
versus transfer bytes, staging peak, CPU/memory, repair duration and upgrade/recovery outcomes.
Public NAT claims require hardware/field measurements with population and pair definitions;
netns results are regression evidence. Comparisons with iroh/Tailscale require the same
matrix, environment and methodology. Report connection success separately from direct rate.

For relay operations, model installed nodes × reservations/keepalives plus relayed fraction ×
transfer bytes, regional capacity and abuse reserves. Account for idle nodes and long transfers,
not just successful direct demonstrations. Publish service support/quotas separately from
protocol capabilities; no unresolved funding assumption may hide inside “zero configuration.”

## v0 two-machine demo (0028)

**Status: performed on a real LAN, 2026-10-21** (build b0a8c68, linux/amd64, static). Two
alphac test-cluster machines (Ultra 9 285HX, 10G LAN, `192.168.1.0/24`, no NAT between them;
other workloads were running): A = alphac-2 `nu daemon` on UDP 4271, `nu add -r site`
(`index.html` + 50,000,000 random bytes). B = alphac-3 with a brand-new `NU_PATH`, no
`init`, no config and no daemon:

| Check | Result |
|---|---|
| `nu get <link> -o out` (link with `peer=` + `addr=`) | 2 files, 47.7 MiB, 0.17 s wall, 26 MB max RSS; SHA-256 identical |
| Same with a link holding only CID + `peer=` (fresh repo; A found via mDNS, 0060) | identical bytes |
| B `nu daemon` gateway, `Host: <cid>.localhost` | `index.html` rendered; 50 MB blob 200; `/tree/<cid>/` listing with "open as site" |

A routed (non-LAN) run is still to do. Earlier, the same steps ran on one machine with two
`$NU_PATH`s (2.9 MB tree in 0.2 s; daemonless `share` → `get` and `doctor --peer` passed).

Needs: two machines that can reach each other directly over UDP (same LAN, or routed with no
NAT between them); the `nu` binary on both (`go build -o nu ./cmd/nu`). Default repo is
`~/.nu`; set `NU_PATH` to use another directory. Everything is 0-config: the first command
initializes the repo.

Machine A (the holder):

1. `nu daemon` — prints the peer id and the swarm addresses (one per interface). Leave it
   running. Expect `swarm /ip4/<A's LAN IP>/udp/4171/quic-v1` among them; if the only
   addresses are wrong interfaces, set `swarm.listen` with `nu config set` and restart.
2. In another terminal: `nu add -r <dir>` — expect `added <cid> <name>` and a
   `share: nu://<cid>?peer=<id>&addr=/ip4/<A's IP>/udp/4171/quic-v1…` line. Copy the link.
   (`nu share <cid>` prints it again at any time.)
3. `nu status` — expect `network: serving, 0 peers connected` (the count rises while B fetches).

Machine B (the recipient, no daemon, no `nu init`):

4. `nu doctor --peer '<link>'` — expect `ok peer: reached <A's id>, round trip <n>ms`. A
   `fail peer: no answer …` means UDP 4171 is blocked on A (firewall) or the address is
   not reachable from B; the action line says what to try.
5. `nu get '<link>' -o <out>` — expect `fetching` then `wrote <cid> to <out> (<n> files, <size>)`;
   `diff -r` against A's directory is empty. Repeat with a single file (`nu add <file>` on A).
6. `nu cat <cid>/<path>` and `nu ls <cid>` on B now answer from B's own store.
7. Browse: `nu daemon` on B, then open `http://<cid>.localhost:8171/` (or
   `http://127.0.0.1:8171/tree/<cid>/`) — expect the directory (an `index.html` renders as a site).

Also try the daemonless holder: stop A's daemon, run `nu share <cid>` on A (it serves in the
foreground and says content is available only while a holder is online), repeat step 5 on
B from a fresh `NU_PATH`, then Ctrl-C the share. Record: OS/arch of both machines, link type
(Wi-Fi/Ethernet/routed), sizes, wall time of step 5 and anything confusing in the output.

## Browser origin-isolation checks (0035)

**Status: not yet run in any browser.** Required by the
[security contract](operations.md#feature-entry-gates) gates; results go in the table
below, one row per browser/OS/version. `scripts/browser-check.sh` adds two copies of
`scripts/browser-check/` (so two root CIDs exist) and prints the URLs; the page runs
B2–B6 itself and shows a result block to paste here. Needs a running `nu daemon` on the
default ports (edit the page's constants otherwise).

| # | Check | Pass when |
|---|---|---|
| B1 | Open `http://<cidA>.localhost:8171/` | The check page renders (the browser resolves `*.localhost` to loopback without a hosts entry). If it does not load at all, record "no `*.localhost`" and continue with B7. |
| B2 | Page fetches `http://localhost:8171/tree/<cidA>/` and `http://127.0.0.1:8171/tree/<cidA>/` | Both reads fail (no CORS); the page reports "blocked". |
| B3 | Page fetches `http://127.0.0.1:5171/api/v0/status` | Read fails; `nu status` still works from the CLI. |
| B4 | Page fetches `http://<cidB>.localhost:8171/` and reads an iframe of it | Fetch blocked; `contentDocument` inaccessible. |
| B5 | Page POSTs to `/api/v0/add` as JSON and as `text/plain` with target `/nu-browser-check-must-not-exist` | `nu operations` lists **no** `add` operation for that path; JSON POST fails in the page (preflight refused). |
| B6 | Page sets a cookie with `domain=localhost`, a host-only cookie and a `localStorage` key; then open `http://<cidB>.localhost:8171/` and `http://localhost:8171/` | Site B and the bare origin show none of A's values. A `domain=localhost` cookie that leaks is a finding to record (the gateway never reads cookies, so it is not a daemon risk, but it bounds what site content may trust). |
| B7 | Open `http://127.0.0.1:8171/tree/<cidA>/index.html` and `/tree/<cidA>/` | The HTML is a download (never rendered); the listing renders with an "open as site" link. The gateway root `/` renders. |
| B8 | macOS (case-insensitive APFS) only: `nu get <cid> -o out` of a tree holding `a.txt` and `A.txt` (the script builds it when the local filesystem allows; otherwise add it on Linux and fetch by share link) | `nu get` prints `skipped A.txt … maps onto existing entry a.txt`; `out/` holds one file with `a.txt`'s content. |

Browsers to cover before the 0028 evidence is complete: Chromium (Linux or macOS),
Firefox (Linux or macOS); Safari on macOS is required before 0043. Record the browser
version, OS, the pasted result block, and anything that needed a hosts-file entry.

| Browser | OS | Version | B1 | B2 | B3 | B4 | B5 | B6 | B7 | B8 | Notes |
|---|---|---|---|---|---|---|---|---|---|---|---|
| (none yet) | | | | | | | | | | | |

## Release evidence

Index results for [milestone](../spec.md#milestones) exits, the [operational acceptance
scenarios](operations.md#acceptance-scenarios), interoperability, fault/security review,
macOS/Linux portability, migration and recovery. User testing must include fresh users who
have not read the protocol docs. Record confusing errors and manual workarounds as failures
of the intended easy path, even when an expert can eventually complete the transfer.
