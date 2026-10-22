# Validation and measurements

[Spec overview](../spec.md) · [Product brief](product.md) · [Operations](operations.md) · [Networking](networking.md)

**No field NAT-traversal or distributed scale results are recorded yet.** Local 10 GB
and 1M-entry hardening measurements are recorded below; they do not establish field
success. Specification figures remain hypotheses and sizing targets unless explicitly
measured. Record reproducible results here (or link reports indexed here), including
negative results.

## Authenticated Nu path discovery (0017, 2026-10-21)

Implementation: `p2p/disco`; [wire/security contract](networking.md#path-v1-control-and-probe-security-contract-0017)
and [owner integration / deliberate limits](../p2p/disco/README.md). Baseline
`677aab8` plus 0017; stock quic-go v0.64.1, Go 1.26.0, rootless Linux namespaces.
These are **bounded fixture smoke measurements**, not field success rates.

Reproduce:

```
go test ./p2p/disco -v
go test -race ./p2p/disco -count=1
go test -tags natlab ./p2p/disco -run '^TestNATLabDisco$' -v -count=1
```

| Real UDP/nftables fixture pair | Both endpoints direct after Open | Direct recovery after address change | Verified transfer |
|---|---:|---:|---|
| EIM/EIF ↔ EIM/EIF | 11.5 ms | 1.039 s | 4,194,304 bytes; prior progress 131,072 bytes preserved |
| EIM/EIF ↔ EIM/APDF | 29.1 ms | 1.047 s | 4,194,304 bytes; prior progress 131,072 bytes preserved |

Each case starts on a framed relay leg, upgrades with exporter-authenticated probes,
then flaps an interface and changes its inside address `10.0.0.2` → `10.0.0.3`
and external address `198.18.0.20` → `198.18.0.22` while transferring. The actual
shared wildcard UDP socket, inner QUIC connection and application stream remain
open; every 16 KiB chunk is byte-verified. Recovery includes the deliberately
one-second-paced control update. The relay is a process-bridged Unix stream,
**not** an end-to-end circuit reservation/reconnection test; direct traffic crosses
real namespaces, routers and nftables NAT/filter rules. Candidate mappings are
primed by an observer-bound packet before exchange, as reflexive discovery would
do. Without priming, nft conntrack can cache an initial no-DNAT result, leaving
all eight early probes unanswered; that fixture artifact was observed and is not
counted as a successful initial traversal. APDF is on one side; both mappings
are EIM. No public/IPv6/router-mapped traversal matrix is claimed.

The fast loopback test also withdraws/re-advertises candidates, transfers while
relay-only, executes the D12 RestartProbing hook and reopens disco on the **same**
inner TLS connection with fresh per-control-stream nonce-bound keys. Focused
negatives cover MAC forgery, replay, unauthorized source/destination, unsolicited,
wrong-destination and expired pongs, expired candidates, forbidden address classes,
peer/global output rates, PCP PEER restrictions, healthy-path hysteresis and
continuous 30 s / 2 min / 10 min retry scheduling without long sleeps. Both message
decoders have fuzz seed coverage. The focused race run passed.

Pre-commit `go vet ./... && staticcheck ./... && go test ./... &&
./scripts/depguard.sh` passed. Independent security review is **not yet complete**;
it is requested for the D11 path contract and mandatory owner destination policy
before public enablement. Daemon/CLI integration is not installed here. A 1 GB
soak, long/field NAT matrices (0024/0025), physical socket replacement, constrained
MTU and relay reconnection remain unverified; small probes alone cannot establish
the required 1200-byte inner packet budget. Prediction/spraying/profile negotiation
and datagram relay optimization are separate extension tasks, not silently enabled
by the baseline protocol.

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
Initial measurements below are retained; the [profiling follow-up](#nu-path-throughput-profiling-0015-2026-10-21)
removes the direct receive/copy queue and isolates the remaining stock observer cost.
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
## Amino Kademlia service (0023)

Local checks: `go test ./kad ./fetch` passes, including codec roundtrip/malformed
input and bounds, XOR bucket limits, and an in-process three-node QUIC DHT scenario:
server nodes announce a BLAKE3 multihash, an independent client finds the provider,
client-only mode registers no handler, and FIND_PEER lookup returns the target. The
message-decoder fuzz target `kad.FuzzDecode` has fixed seeds and runs in the normal
package suite.

Interop (both directions): `cd experiments && go test -run
'^TestAminoProviderInteropBothDirections$' -count=1 -v ./kad` passed locally on a loopback
Nu + stock go-libp2p v0.50.0 QUIC-v1 network. Nu BLAKE3 provider → independent go DHT
`FindProvidersAsync`, and go SHA-256 provider → Nu `GET_PROVIDERS`. Go's server routing
table is explicitly seeded with Nu in this test.

**Kubo v0.40.0 Docker acceptance (both directions):** `cd experiments && NU_DOCKER=1
go test -run '^TestDockerKuboAminoProviderInterop$' -count=1 -v ./kad` passed on
2026-10-21 (about 3 s). Image is digest pinned to the interop runner's
`ipfs/kubo:v0.40.0@sha256:e08c602bf97f138a0ec5b42155b4fd4532852919250b11b4aadbea2bf42c0e10`.
The disposable container uses `Routing.Type=dhtserver`, QUIC-v1 only, bootstrap=Nu;
the reverse query is `ipfs routing provide` of a stored BLAKE3 CID. Nu checks the Kubo
provider via `GET_PROVIDERS`, and Kubo `ipfs routing findprovs` returns Nu's PeerId.
This is an isolated Docker bridge test, **not** public Amino or a throughput/replica test.
The test disables Kubo Swarm address filters only inside that disposable bridge and
uses synthetic `11.0.0.0/24` Docker addresses so LAN referrals pass routing admission;
no public bootstrap peers are configured. Identify confirms Nu server advertises
`/ipfs/kad/1.0.0`.

The first Docker attempts exposed an Amino stream-lifecycle issue: go-libp2p-KAD can
send multiple length-delimited requests on one `/ipfs/kad/1.0.0` stream, while Nu
handled only one request and closed/reset the stream. Nu now serves a bounded sequence
(up to 64 messages) until clean EOF; client RPCs half-close and drain the reply stream.
Kubo also uses FIND_NODE with arbitrary provider-key bytes, not only PeerIds; Nu now
accepts generic bounded DHT keys there. After these fixes, the Docker test passed both
provider directions.

**Live Amino / Q2:** the implementation compares with the interop driver's three pinned
QUIC-v1 Kubo bootstrappers and identifies them before providing, uses the remote
Identify-reported observed UDP address for provider records (instead of `0.0.0.0`), and
asserts a client-only Nu host does not advertise `/ipfs/kad/1.0.0`. On 2026-10-21,
`cd experiments && NU_AMINO=1 go test -run '^TestLiveNuAminoProviderKeys$' -count=1 -v ./kad`
passed all three independent Nu announce/find cases:

| Key | CID/multihash | Provide | Independent Nu find | Providers returned |
|---|---|---:|---:|---:|
| SHA-256 control | `bciqoiu4ttomaotanbabsgl4d4vvxmwxj6jqzdjnbezlyu4xob3p32vy` | 19.8 s | 11.4 s | 3 |
| Random BLAKE3 | `bdyqfcepjvhblop2pihorxh3z3xv62peon4qpw2hgya4ndvl25qcqw5y` | 35.9 s | 16.6 s | 3 |
| Follow-swarm BLAKE3 | `bdyqjy3do72eb4nzsppb6kaqkxaxxhlthl5s7m4pjhl4dkjmplcgysta` | 19.8 s | 12.1 s | 3 |

Nu independently found its own provider PeerId in all cases. This is a single bounded
run, not a replication-fraction measure; the public lookup returned three provider
records per key.
The exact deterministic recipe was `BLAKE3("nu.follow\\0did:plc:ewvi7nxzyoun6zhxrhs64oiz\\0amino-spike-dataset")`.

Public `FIND_PEER`: `cd experiments && NU_AMINO=1 go test -run
'^TestLiveNuAminoFindPeerPublicBootstrap$' -count=1 -v ./kad` passed on 2026-10-21.
A Nu node seeded with two pinned public Amino peers, but not the target, found the third
pinned Kubo bootstrap PeerId and a UDP/QUIC-v1 address. This demonstrates one public
Kubo peer lookup, not broad global coverage. Some referrals were stale/unreachable or
IPv6 (unsupported by this single-socket live harness); one response exceeded the
current decoder limit, but the requested peer was still returned.

**Receive buffer:** quic-go still reports 416 KiB available vs 7168 KiB requested.
`net.core.rmem_max/default` are 212992; `sudo -n sysctl ...` failed because sudo requires
a password, so no system setting was changed. Public runs remain subject to that limit
and remote peer resource policies. The Docker test, unlike live public, isolates these
variables. Owner-side daemon wiring of AutoNAT reachability, bootstrap lifecycle,
pinned-CID enumeration and periodic reprovide is not installed; private mode is rejected
by the service but the owner must wire its construction. Provider replica fraction and
long-term expiry/republish remain unmeasured.

## Nu-path throughput profiling (0015, 2026-10-21)

**Optimized, but the original ≤~5% versus uninstrumented raw criterion is still open.**
The remaining measured gap is approximately **16–22%** with the current stock-stack
CID-observer design. No fork, wire-format change, weaker CID/source checks, disabled
CID rotation, or relaxed inner MTU was used to make the numbers look better.

### Method and fair baselines

Before code: `d4b7630` (the merged packet core is unchanged from `dbd92b3`); after:
this profiling/optimization change. Same AMD EPYC 9684X host, 192 logical CPUs visible,
Linux 5.15.0-176-generic, Go 1.26.2 linux/amd64, quic-go v0.64.1, `GOMAXPROCS=4`,
no CPU affinity or exclusive-machine guarantee. UDP loopback, no injected loss, same
4 MiB unidirectional stream operations over one established libp2p-TLS QUIC connection
per case. Setup is outside timing; stream EOF byte counts are checked. The kernel
caps the requested receive buffer at 416 KiB; the quic-go warning is retained.

- **Raw** is unwrapped UDPConn, **1200-byte fixed packets**, PMTU discovery off,
  eight-byte local CIDs, 1 MiB stream / 4 MiB connection receive-window caps. These
  settings are identical to VirtualDirect. The original benchmark **already used
  this same-packet-size baseline**: PMTU size did not explain the original 44% result.
- **RawObserver** adds a no-op public qlog recorder to Raw. It does no routing,
  serialization, wrapping, socket hand-off or per-event application work. Stock
  quic-go still constructs the events before invoking it. This isolates the mandatory
  observer cost; it is a diagnostic control, **not a silent replacement of Raw**.
- **RawPMTU** is an additional stock-packet-policy control, with the default initial
  size and PMTU discovery enabled. It is not the wrapper-only comparison.
- All cases report GSO enabled. Final fixed-size runs averaged **1199 sent bytes per
  QUIC packet**; RawPMTU averaged **1440**. CPU stacks include `sendPacketsWithGSO`;
  existing relay GSO segmentation tests remain enabled. VirtualDirect had **zero
  adapter receive-queue drops**. QUIC reported approximately 1–5 lost packets per
  4 MiB operation in the final run (not a physical-link loss-rate measurement).

Reproduce from `experiments/` (benchmark remains `slow.Skip`-gated):

```sh
NU_SLOW=1 GOMAXPROCS=4 go test -run '^$' -bench '^BenchmarkProductionPaths$' -benchtime=1s -count=3 -benchmem ./nupath
NU_SLOW=1 GOMAXPROCS=4 go test -o /tmp/nu-path.test -run '^$' -bench '^BenchmarkProductionPaths/VirtualDirect$' -benchtime=2s -benchmem -cpuprofile=/tmp/nu-path.cpu -memprofile=/tmp/nu-path.mem -mutexprofile=/tmp/nu-path.mutex ./nupath
go tool pprof -top /tmp/nu-path.test /tmp/nu-path.cpu
go tool pprof -top -alloc_space /tmp/nu-path.test /tmp/nu-path.mem
go tool pprof -top /tmp/nu-path.test /tmp/nu-path.mutex
```

### Before/after results

Unprofiled one-second runs, three repetitions; MB/s is decimal. Before was also
rerun using the retained **unmodified** binary, not reconstructed by selectively
turning off correctness features. Ranges show shared-machine variation, not p95s.

| Case | Before MB/s median (range) | Final MB/s median (range) | Final bytes/op; allocations/op (median) |
|---|---|---|---|
| Raw, fixed 1200 | 740 (721–767) | **722 (695–741)** | 475,214; 15,316 |
| VirtualDirect, fixed 1200 | 530 (518–549) | **564 (555–567)** | 2,855,246; 44,561 |
| RawObserver, fixed 1200 | — | **564 (555–567)** | 2,805,055; 42,620 |
| RawPMTU | — | **875 (804–917)** | 400,310; 12,732 |

Before virtual allocated a median **3,139,047 bytes / 55,551 objects per operation**.
The final implementation reduces that by **9.0% / 19.8%**, respectively. In the final
confirmation set, virtual throughput improved **6.3%** and its raw-relative deficit
fell from **28.4% to 21.9%**. An earlier complete three-repeat optimized set achieved
**603 MB/s** virtual (578–608), versus **716 MB/s** Raw (707–760), a **15.7% gap**;
RawObserver was **595 MB/s** (571–601). That is the best completed set, not a claim
that the final slower confirmation should be discarded. The earlier 100 ms/44%
result is not a stable before/after baseline for these longer runs.

A separate one-second **ECN-disabled control** (`QUIC_GO_DISABLE_ECN=true`, n=1) gave
Raw **727**, RawObserver **592**, VirtualDirect **591 MB/s**. Thus inner ECN suppression
is not hiding a missing-GSO or ECN-related performance explanation. RawPMTU's additional
advantage is the separate, intentional 1200-byte D11 packet-budget tradeoff.

### Profile findings and changes

- Before profiles (`/tmp/nu-vdirect-before.*`, `/tmp/nu-raw-before.*`) showed that
  approximately **70% of virtual allocated space** was in quic-go packet/frame/metric
  qlog construction, despite our observer doing no log serialization. Virtual cost
  was ~3.12 MB/op versus raw ~0.47 MB/op. Dropping events in our recorder is too late:
  `logShortHeaderPacketWithDatagramPayloadChecksum`, `handleShortHeaderPacket.func2`,
  `toQlogFrame` and `qlogMetricsUpdated` have already allocated their arguments.
- The direct receive path now runs kernel `ReadBatch` **directly into quic-go's
  buffers**, with no separate UDP receive goroutine, intermediate pooled packet copy,
  or per-packet channel send/receive. Relay frames alone use the bounded receive queue;
  enqueue wakes an idle physical read via a private deadline without changing the
  caller deadline. Filtering compacts by copying across holes, never swapping buffers
  out from under quic-go's indexed ownership bookkeeping.
- CID map keys are inline comparable values, not freshly allocated header strings.
  Virtual UDP source addresses are created per peer/batch rather than per datagram.
  Linux GSO metadata uses a borrowed validated cmsg view instead of allocating a
  parsed-message slice and rebuilt control buffer. OOB filtering and GSO splitting
  semantics are unchanged.
- After profiles (`/tmp/nu-vdirect-after.*`) no longer show CID-string, parsed-cmsg or
  receive-pool allocation hot spots. `runtime.memmove` fell from **3.9% to 2.3%** of
  sampled CPU; qlog construction now accounts for roughly **78% of allocated space**.
  Our CID recorder itself was **1.2%** of sampled CPU. Mutex delay was only **24 ms
  before / 23 ms after** over ~3.2 s profile captures, not the dominant cost. The
  original block profile mostly counted expected idle/network/channel waits and was
  not interpreted as equivalent CPU cost. Profiled throughput is not mixed into the
  unprofiled table.

### Why stop here / proposed acceptance change (not adopted)

The final **no-op RawObserver control is already ~22% below Raw**, with VirtualDirect
essentially matching it. This is the measured floor of the **current architecture**,
not a mathematical claim that every conceivable stock-quic-go architecture is slower.
In v0.64.1 the public per-connection CID lifecycle information is carried through the
broad qlog event interface; there is no public CID-only event-construction filter.
`NEW_CONNECTION_ID` / retirement frames are encrypted, so the wrapper cannot just sniff
UDP headers. Learning by source endpoint instead of CID violates D11 when outer and
inner QUIC share an endpoint; disabling the observer after handshake breaks rotation.
The transport-global CID generator has no connection/peer argument to bind later IDs.

To pursue the original ≤5% total target, request an **upstream** CID-issued/retired
callback or selective event-construction API, then remeasure; no local quic-go fork
was introduced. Per-peer QUIC Transports with self-routing CIDs are another possible
architecture, but change the current single-dispatcher/Host design and were not
silently substituted during this optimization.

Proposed spec split, requiring an owning decision: (1) measure adapter overhead
against identically sized **RawObserver** and retain the ~5% incremental target;
(2) always publish uninstrumented Raw and RawPMTU alongside it, keeping the currently
~16–22% CID-observer cost and fixed-packet-policy cost visible; (3) keep the original
end-to-end ≤5% goal open pending an upstream API or an approved architecture change.
**Changing only to “same packet size” does not close this gap: that baseline was already
used.** No acceptance criterion has been weakened or marked complete here.

Correctness verification retained: normal suite/depguard, race-enabled magicsock/qpath,
rootless two-NAT qpath scenario, real go-libp2p rejection/pass-through, decoder fuzz seeds,
and Darwin arm64 compile. Added one fast regression for caller-buffer ownership across
a filtered batch, relay wakeup of an idle fd read, unchanged logical deadline, next-read
reset, and small-buffer virtual-source demux. No long fuzz/loss/soak run was added.
Two initial full-suite runs hit `nutest.TestRestartMidTransfer`'s unrelated one-second
startup assertion (13.03 s / 1.73 s); its isolated retry passed in 0.14 s and the final
full suite passed (`nutest` 0.38 s). The changed packet-core packages are absent from
that test's dependency graph; no nutest/node/store code or timing threshold was changed.

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
| B9 | Public seed only: the page on `https://<cidA>.<domain>/` fetches `https://<domain>/.well-known/rasl/<cidA>` cross-origin and hashes it | Readable (CORS `*` on RASL only) and the SHA-256 digest equals the CID's; B2 on the same page still reports the bare origin's `/tree/` as blocked. |

Browsers to cover before the 0028 evidence is complete: Chromium (Linux or macOS),
Firefox (Linux or macOS); Safari on macOS is required before 0043. Record the browser
version, OS, the pasted result block, and anything that needed a hosts-file entry.

| Browser | OS | Version | B1 | B2 | B3 | B4 | B5 | B6 | B7 | B8 | Notes |
|---|---|---|---|---|---|---|---|---|---|---|---|
| (none yet) | | | | | | | | | | | |

**Public seed rerun (0043).** The same page runs against a real public domain: deploy a
seed per the [runbook](operations.md#public-seed-deployment-runbook) (wildcard DNS and
certificate, or Caddy on-demand TLS), then `PUBLIC=<domain> scripts/browser-check.sh` on
the seed (it marks both trees public and prints the `https://` URLs) and open site A from
a machine **without Nu** in Chromium, Firefox and Safari. The page detects the public
origin: B2 reads `https://<domain>/tree/<cidA>/`, B3/B5 still target the visitor's own
`127.0.0.1:5171` (a public page must not reach a visitor's local daemon; run them on a
machine that *does* run a daemon too), B4/B6 use `<cidB>.<domain>`, B7 is the bare
`https://<domain>/tree/<cidA>/index.html` download, and B9 is added. Also record: the
listing's "open as site" link lands on `https://<cidA>.<domain>/`; `https://<domain>/`
with `Host` games (`curl -H 'Host: evil.example' https://<ip>/ -k`) is `403`; a held but
unmarked CID is `404` on `/tree/`, `/blob/`, RASL and its site origin; `nu public rm`
makes the site `404` on reload. Automated coverage of those rows:
`gateway.TestPublicListener`, `daemon.TestPublicListener` (TLS with a test CA, real
daemon, client that trusts the CA and resolves the domain to the listener). **Status:
not yet run against a public domain** — no domain, certificate or browser is available to
the agents; this is the human step that gates 0043's browser claim.

| Browser | OS | Version | Domain/TLS | B1 | B2 | B3 | B4 | B5 | B6 | B7 | B9 | Notes |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| (none yet) | | | | | | | | | | | | |

## Placement and readiness (0046)

Fast tests per the v0-era policy (one test per row or a table; scale, rescan cost and
large-cluster evidence belong to 0050/0068). Commands: `go test ./cluster ./replica
./cmd/nu`.

| Rule / row | Test | Shows |
|---|---|---|
| Placement vectors | `cluster.TestPlacementVectors`, `TestPlacementRules` | scores, ranking, owners with/without spread and the epoch are fixed bytes; draining/client members own nothing; adding a member moves only its share |
| R1 (E10/W2) | `cluster_test.TestPlacementReadiness` | a head acknowledged by all 3 storage members (`metadata` wait, 3 acks) whose content nobody has: content not ready, `ready` wait times out |
| E6, placement sync | same | 3 routed private members, `replicas: 2`: each member holds the structure and exactly the blobs it owns; `ready` once every owner's fresh report confirms; `held` wait names holders, non-member holder fails |
| R3 (E1/W1) | same | a wait on T keeps T after the head moves, ends in timeout with `superseded_by`, while the new head becomes ready on its own |
| R4, R5, R7, R8, E8 | `cluster.TestReadyMatrix` (fake clock) | other-epoch/policy report → `missing: epoch`; clock past `evidence.fresh` → `unknown`, not degraded; incomplete surplus holder → still ready, not in `extra`; complete non-owners only → not ready, `extra` shown; fresh contradiction after ready → `degraded` |
| R6 (E5) | `TestPlacementReadiness`, `TestReadyMatrix` | `replicas: 4` on 3 members and `spread: zone` without zones fail a ready wait fast with the shortfall; the team default warns instead of sharing failure domains silently |
| Heartbeat reports | `cluster.TestReportDelta` | deltas apply in sequence, a missed heartbeat forces a full pull, removals propagate |
| R15 (S5) | `replica.TestFirstHeldProtection` | never-ready dataset, five head changes with unavailable content: the last fully held view stays pinned, two heads staged, later heads observed only |
| R17 (S7) | `replica.TestPressurePauses` | store limit during an update: `paused: insufficient_storage` with bytes needed, protected pin and content intact |
| One-node `k=1` | `cluster_test.TestLocalReady`, `cmd/nu.TestDatasetWaitCLI` | local dataset ready at `required: 1`, epoch `local`; CLI `update`/`wait`, timeout exit 6 with the snapshot |
| `release.auto=false` | by construction | nothing in 0046 unpins held or ready versions; only never-held staged heads are dropped |

Not yet covered: collection floor placement (`replicas` > 0 on collections), E8 under a
real partition in nutest (the fake-clock table covers the rule), and the Linux isolated
lab with placement traffic.

## Version history and archives (0047)

Scaled to the v0-era test policy: a few fast tests per acceptance bullet (Linux only).

| Bullet / rule | Test | Shows |
|---|---|---|
| One-copy archive preset, two versions, exact old bytes without PDS or other nodes | `node.TestOneNodeArchive`, `cmd/nu.TestCLIArchive` (daemonless) | `--archive` writes the default schedule with `replicas: 1`; `--source` imports the first version; `update` re-adds; `cat`/`get -o` of `<ds>@<tid|label|time>` return the old bytes; `/c/<ds>@<label>/<path>` redirects to `/tree/<cid>/<path>` |
| R9 (E6), E8 archive-wide | `node.TestOneNodeArchive`, `cluster_test.TestClusterArchive` | one node: content and archive state `ready`, `required: 1`, one confirmation; two members (`replicas: 1`): `wait --condition archive-ready` needs both owners' live-set reports (`required: 2`) |
| New incomplete version vs established availability (E8) | `cluster_test.TestClusterArchive` | a head whose content exists nowhere makes the archive not ready while the older version stays `retained-complete`; reading the new version is `unavailable`; the old one lists through placement |
| Rollback as a new head | `TestOneNodeArchive`, `TestClusterArchive` | new head and version with `restored_from`; the restored version's identity and label unchanged; metadata and content are separate results |
| Incomplete/never-replicated/pruned never claim retrieval; skipped versions | `node.TestArchiveRetentionAvailability` | a head with unheld content is `observed`, superseded outside the live set it is `skipped`; a pruned version is `pruned`, reads (by TID or by a time selecting it) and rollback fail `unavailable` naming it, never a neighbour |
| Shared references prevent unsafe deletion; dry-run | same | the dry run lists the candidate with free and shared bytes and changes nothing; after prune and GC the other dataset and the live version still read the shared file; networked prune is dry-run only |
| Labels and held copies survive budget pressure | `node.TestArchiveBudgetPressure` | an import over `store.limit` fails `insufficient_storage`; after GC the labeled and held versions are `retained-complete` and read |
| Explicit missing-version reacquisition | `TestArchiveRetentionAvailability` | labeling pruned content fails without `--reacquire`; with it the version is live and still not retrievable until its content is complete again |
| Immutable identity vs annotations, collisions, compaction | `cluster.TestVersionMerge` | an annotation copy merges by (hlc, signer); a later self-authored redefinition of the same TID is a reported conflict, never merged; a winning tombstone compacts; label syntax excludes TIDs and times |
| Retention schedule, lookup without fallback | `cluster.TestRetainAndSelect` | GFS buckets from the newest version, labels and head always live, `max_bytes` drops the oldest unprotected and reports protected excess; deterministic live-set identity; a time before history is an error; a duplicated label is ambiguous |
| Crash consistency | `TestOneNodeArchive` (restart) | head and version are one signed commit; history, labels and availability survive a restart |

Not covered: macOS/APFS crash, sleep, Unicode and case tests (no macOS runner here;
extraction's case/normalization collision handling is X1–X6), process kill between the
import and the head write (the import is unpinned and falls to GC after the grace),
authenticated import-manifest export (needs 0042's manifests) and historical identity
evidence for external sources (0045, 0054).

## Release evidence

Index results for [milestone](../spec.md#milestones) exits, the [operational acceptance
scenarios](operations.md#acceptance-scenarios), interoperability, fault/security review,
macOS/Linux portability, migration and recovery. User testing must include fresh users who
have not read the protocol docs. Record confusing errors and manual workarounds as failures
of the intended easy path, even when an expert can eventually complete the transfer.

### Post-v0 hardening (0068, 2026-10-21)

**Bounded local campaigns passed with the limitations below; this is not a public-preview
exit declaration.** [Harnesses and exact reproduction commands](../experiments/hardening/README.md).
Base `cf98ad8` plus this hardening change; Go 1.26.2, Linux 5.15.0-176-generic,
amd64 AMD EPYC 9684X (192 logical CPUs visible, shared host, no pinned cores).
Measurements use `GOMAXPROCS=4`; fuzzing uses two workers per target, four concurrent
targets. Disk free was 87 GiB before builds, 85 GiB after campaigns/TempDir cleanup.
Native arm64/macOS execution is **CI-only and unobserved**; no qemu-aarch64 binary was
available. No production parser/store/transport bug was found or fixed in this campaign.

| Item | Result / boundary |
|---|---|
| Fuzzing | **Pass:** all 37 discovered main-module targets, 60 s each, 75,645,569 executions; no crashers. Nightly runner defaults to 10 min/target, continues after failures and uploads logs plus Go's minimized `testdata/fuzz` seeds for review/commit. Default tests only replay seeds. |
| Cross-implementation | **Pass with recorded oracle limitation:** 92 applicable DASL fixtures, independent CAR/SHA-256/BLAKE3 checks, indigo data model/repo checks and Rust bao-tree 16 KiB vectors. Prime negative-integer divergence and absence of upstream CAR fixtures are detailed below. |
| Hash/Bao benchmarks | **Measured amd64; arm64 CI-only.** Opt-in benchmarks and three local repetitions below. |
| MST fanout / 1M lookups | **Pass at measured scale; fanout 16 retained as a compromise**, not proven optimal. Three repetitions of real Go-source names, node_modules-like names and a flat 1M-entry tree. |
| 10 GB scale | **Pass for local streaming and corrupt-provider recovery. Near-sum throughput NOT met:** 47.27 MB/s with two honest + one corrupt provider versus 73.72 MB/s single-provider; no independent-link field measurement. |
| Store crash injection | **Pass within scope:** 48 child-process SIGKILL trials across add/pin/GC/ordered pin-switch workloads; integrity, durable pins, complete blob/outboard hashes, accounting and post-GC orphan removal checked on reopen. Not power-loss or atomic named-head switch evidence. |
| Platforms | Nightly/on-demand native matrix: `macos-14` arm64, `macos-15-intel` amd64, `ubuntu-24.04-arm` arm64; asserts GOARCH. No native non-amd64 result is claimed until those jobs run. |

All new expensive tests/benchmarks call `slow.Skip` (`NU_SLOW=1`). The dedicated
`hardening` workflow is nightly/on-demand only, not a PR job. Local raw logs remain
in ignored `.hardening/{fuzz,formats.log,indigo.log,hash-bench.log,mst-final.log,
crash.log,scale.log}`; workflow publication/execution is unobserved because this branch
has not been pushed. A scale runner with less than 35 GB free explicitly skips the
10 GB scenario and must not be counted as a scale pass.

#### DASL and independent format compliance

Upstream `hyphacoop/dasl-testing` revision **fe880a1ef6891c22af02695d1d76430b9d8bfc91**
was still HEAD when checked. All eleven fixture files are present, unmodified. The
matrix includes every case; basic/dag-cbor/dasl-cid tags apply to Nu, other-profile-only
CBOR-Core/dCBOR/CDE/RFC cases do not. `invalid_out` fixtures are unrepresentable in
DRISL and are rejected by its strict decoder; encoder rejection is also covered by
`core/drisl.TestEncodeRejects`. This is not a claim of those other CBOR profiles.

| Fixture | Applicable cases, all pass | Other-profile only | Independent valid roundtrips |
|---|---:|---:|---:|
| cid | 12 | 0 | 2 (SHA-256 and BDASL BLAKE3) |
| concat | 1 | 0 | 0 |
| floats | 18 | 5 | 3 |
| indefinite | 4 | 0 | 0 |
| integer_range | 9 | 2 | 5 pass, 1 external divergence |
| map_keys | 6 | 0 | 2 |
| numeric_reduction | 0 | 4 | 0 |
| short_form | 30 | 1 | 6 |
| simple | 5 | 0 | 1 |
| tags | 3 | 0 | 0 |
| utf8 | 4 | 1 | 3 |
| **Total** | **92** | **13** | **22 pass, 1 external divergence** |

- **Independent DAG-CBOR:** go-ipld-prime **v0.24.0** decodes/re-encodes all valid
  applicable fixtures. The sole divergence is `3bffffffffffffffff` (CBOR −2^64): its
  generic decoder wraps to zero (`00`), while Nu preserves the full DRISL integer.
  The harness explicitly recognizes this exact pinned-oracle defect, logs it, and
  still requires Nu's byte-exact result. No broad error/fixture exemption is used.
- **CAR:** upstream DASL has **no CAR fixture files** at this revision. Supplemental
  `TestCARBothDirections` checks byte-identical CAR v1 output and independent reading
  with go-car **v0.6.1-0.20230509095817-92d28eb23ba4**, for both raw SHA-256 and BLAKE3
  payloads, including corrupted-payload rejection by Nu. Existing `core/car` tests
  cover duplicate/unreferenced blocks, truncation and deliberate CARv2 rejection.
  This supplements, rather than invents, an upstream CAR compliance suite.
- **Indigo:** **ff87442ff936** (`experiments/go.mod`) and Nu agree on all three
  JSON/CBOR/CID fixtures, five valid and twelve invalid data-model cases. The existing
  500-record signed-repo load/verify and 30 randomized MST insert/delete/inversion
  cross-checks also passed (0.047 s total). Format tests took 0.020 s.
- **Bao:** cargo **1.96.0**, Rust bao-tree **0.16.1**, BLAKE3 **1.8.7**; committed
  generator/lockfile and `testdata/vectors/bao/bao-tree-16k.json`. All ten boundary
  sizes (empty through 278,535 bytes), ten roots/outboards and thirty full/first/last
  slices matched at 16 KiB groups; generated slice bytes also verify in Nu. Headers
  are normalized explicitly: bao-tree omits Nu's LE64 size prefix. This is independent
  byte-parity evidence, not a runtime iroh dependency.

#### Throughput and directory shape

Eight-MiB in-memory input, one-second benchmarks, three repetitions, no disk/network
I/O. BLAKE3 **9,003–9,103 MB/s** (median **9,055**); Bao pre-order outboard generation
at 16 KiB groups **2,958–3,121 MB/s** (median **3,104**). Allocations respectively
about 110 kB/1,082 allocs and 49 kB/1,025 allocs per operation. These are the existing
Go implementation's costs, not a zero-allocation claim. Arm64 benchmarks are wired
into native CI but have not been observed.

MST measurements use the production directory codec with only `LevelBits` changed.
Real input: `/usr/local/go/src`, **12,788 entries / 1,324 directories** (no local Linux
kernel checkout found). Synthetic node_modules-like: **100,000 entries / 1,000 dirs**;
flat synthetic: **1,000,000 entries / one dir**. Values are identical five-byte file
entries, not actual file contents. Each run checks 1,000 deterministic lookups with a
fresh tree handle (cold decoded-node cache, **memory** block store, not cold disk).
Reported build/p50/p95 are medians of three repetitions; sizes/read counts are stable.

| Shape | Fanout | Build (ms) | Encoded MB / blocks | Lookup p50 / p95 (µs) | Reads / bytes per lookup |
|---|---:|---:|---:|---:|---:|
| Go source | 16 | 27.3 | 1.086 / 2,088 | 14.4 / 87.8 | 1.18 / 411 |
| Go source | 32 | 21.8 | 1.064 / 1,614 | 7.6 / 57.5 | 1.11 / 471 |
| Go source | 4 | 24.5 | 1.177 / 4,285 | 10.9 / 48.3 | 1.46 / 319 |
| node_modules-like | 16 | 159.9 | 9.645 / 8,235 | 73.7 / 239.3 | 2.23 / 3,082 |
| node_modules-like | 32 | 161.2 | 9.494 / 5,005 | 102.9 / 296.4 | 2.01 / 4,396 |
| node_modules-like | 4 | 193.3 | 10.544 / 27,380 | 45.0 / 96.7 | 3.48 / 1,807 |
| flat 1M | 16 | 1,669.9 | 87.942 / 62,300 | 309.5 / 687.5 | 6.55 / 14,136 |
| flat 1M | 32 | 1,377.8 | 86.485 / 31,268 | 472.1 / 1,097.8 | 5.61 / 22,310 |
| flat 1M | 4 | 2,153.3 | 96.722 / 249,391 | 154.7 / 241.8 | 10.51 / 6,800 |

Fanout 16 holds as a reasonable block-count/read-amplification compromise: 32 saves
only 1.7% encoded space on 1M but decodes more bytes and has worse lookup latency;
4 is faster in this memory-backed lookup test but has four times as many blocks and
60% more block reads than 16. **16 is not a universal speed winner.** Update/diff
costs, cold disk/remote block latency, an actual kernel checkout and real node_modules
payload distributions remain unmeasured; no format default was changed. Maximum
1M-node sizes were 15,876 / 30,606 / 5,077 bytes for fanouts 16 / 32 / 4, below the
1 MiB cap. The million-entry tree itself is built in memory; constant-memory claims
below concern file streaming, not tree construction.

#### 10 GB and process-death scope

One **10,000,000,000-byte** deterministic non-sparse file; all cat/get/fetch outputs
checked for exact length and BLAKE3 digest. Four real nutest nodes, loopback QUIC,
no artificial bandwidth/RTT/loss configuration. Provider A used a copied store; two
other providers used the same local file via nocopy. A fresh client first fetched
from A, then GC removed that copy before fetching from all three with one corrupting
provider. Thus multiple sources do not have independent physical storage/bandwidth.
The machine warned that only 416 KiB UDP receive buffer was available versus 7 MiB
requested. First phases overlapped the end of the bounded fuzz campaign.

| Phase | Seconds | MB/s | Sampled peak Go heap (MB) |
|---|---:|---:|---:|
| Node add + pin (copied, raw blob) | 89.92 | 111.21 | 4.27 |
| Node cat + digest | 21.95 | 455.62 | 4.96 |
| Node get to local file | 62.35 | 160.39 | 2.20 |
| One peer fetch + final digest | 135.65 | 73.72 | 13.93 |
| Three peers, one corrupt, fetch + final digest | 211.54 | 47.27 | 16.35 |

The local add/cat/get heap deltas were **2.27 / 2.88 / 0.12 MB**, below the 48 MiB
asserted bound. Sampling every 5 ms, soft Go memory limit 128 MiB; **not RSS/page-cache
measurements**, and sub-sample transient allocations are not captured. Full scenario
including source generation and verification: **591.85 s**. All temporary data was
removed. The corrupt peer served four responses and was dropped on a Bao verification
failure; correct bytes were recovered. Multi-source rate was **0.641× single-source**,
not near the sum of two honest peers. This negative performance result is retained;
independent-link aggregate-throughput acceptance remains unmet, not waived by the
correctness test's pass. Longer soaks and field deployments remain unmeasured.

Crash campaign: PCG seed 68/1, twelve trials per add/pin/GC/ordered-pin-switch workload,
random 0–20 ms delay after child readiness, actual process kill and reopen. Initial
state has 32 distinct 128 KiB blobs, two durably pinned; add loops stream 16 MiB blobs.
All 48 trials passed in **9.54 s**. SQLite `integrity_check`, complete pinned walks,
full blob/outboard verification, usage-counter recount, empty abandoned staging and
post-GC orphan sweeping are asserted. Orphan files before GC are permitted by the
store contract. **Limitations:** kills are scheduler-timed workload samples, not
exhaustive transaction/fsync cut points; GC can finish before the kill. The switch
workload is caller-side pin-new-before-unpin-old with baseline pins preserved, not
cluster/collection head-switch atomicity. No power-cut, filesystem fault or disk-full
campaign was run. These broader guarantees remain unproven.

Verification before commit: `CGO_ENABLED=0 go vet ./...`, `staticcheck ./...`,
`go test ./...` and `./scripts/depguard.sh` all passed. The separate format-oracle
package passed vet/staticcheck/tests; Rust regeneration matched the golden file;
Python/YAML parsed; a final one-second-per-target runner smoke and a second 48-kill
campaign passed. No main-module dependencies or concurrent owners' production files
were changed. The nightly/native workflows themselves remain unobserved.

## IPFS bridge (0042) — early fixture evidence

The default bridge package tests are offline and fast (`go test ./bridge`): hand-built
fixtures cover verified raw/dag-pb blocks, CIDv0/v1 parsing, UnixFS file/directory/symlink
conversion, tampered CAR blocks, decoder fuzz seeds, and a retry after a source block is
missing. The retry re-runs conversion from the CAR and relies on deterministic content addressing/store
deduplication. A node test persists a running CAR job, closes/reopens `Local`, and confirms
the same operation ID recovers to success; this is a restart/recovery test, not a power-loss
or forced process-kill campaign.

Opt-in Kubo test: `NU_DOCKER=1 go test ./bridge -run TestKuboCARImports -v`, using the
same digest-pinned `ipfs/kubo:v0.40.0` image recorded in `interop/run.py`. It passed locally
(16.4 s): Kubo created a directory with 1,201 generated entries, a 3 MiB file, raw UnixFS
leaves, a symlink, and CARs for CIDv1/raw-leaf and CIDv0 imports; imported file bytes and
symlink target bytes were compared against `ipfs get` output. The large directory exercised
Kubo's sharded-directory representation and conversion completed. This is a local opt-in
fixture result, not a CI run or production acceptance. A synthetic Bitswap `/1.2.0` exchange over two Nu QUIC hosts passes. The opt-in pinned-Kubo
Bitswap test also passes: a Nu host requests a 1 MiB CIDv1 raw block over QUIC-v1 and receives
and verifies the payload (0.87 s locally). That probe uses a static provider result; live Amino
DHT provider discovery against Kubo/the public network is not verified. The CAR import is
exposed through CLI/API and durable operation recovery is tested, but CAR jobs persist the
request/stage/result and replay from the CAR rather than retaining per-block checkpoints.
Cutover readiness and representative-installation reconciliation remain unverified.
