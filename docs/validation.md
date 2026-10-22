# Validation and measurements

[Spec overview](../spec.md) · [Product brief](product.md) · [Operations](operations.md) · [Networking](networking.md)

**No field NAT-traversal or distributed scale results are recorded yet.** Local 10 GB
and 1M-entry hardening measurements are recorded below; they do not establish field
success. Specification figures remain hypotheses and sizing targets unless explicitly
measured. Record reproducible results here (or link reports indexed here), including
negative results.

## Real daemon relay sessions and failover (0020, 2026-10-22)

Implementation: `node/swarm` connectivity owner, `p2p/homerelay`, session transport
and signed identify address seams. Baseline `8111314` plus address-publication
hardening; stock quic-go v0.64.1, Go 1.26.0, rootless Linux
user/network namespaces. These are bounded fixtures, **not field success rates**.

Reproduce from the worktree root:

```
go test -race ./node/swarm ./p2p/host ./p2p/proto ./p2p/proto/circuit ./p2p/homerelay
go test -tags natlab ./node/swarm -run '^TestDaemonNAT(ShareUpgradeAndRelayFailover|RelayMatrix)$' -v -count=1
```

The test builds the ordinary `nu` binary (no test-only transport hooks), starts two
`nu daemon` processes in separate private namespaces behind different routers and
two public `nu relay` processes, configures explicit full-service admission and
exact per-peer probe destinations, and uses CLI `add --raw`, `share`, `get` and
`status`. Share links contain both live circuit hints. A parser test rejects
conflicting destination identities and stacked/unsupported circuit hints.

| Daemon fixture | Result |
|---|---|
| EIM/APDF ↔ EIM/APDF; direct initially blocked | 19,500,000 bytes fetched via a real circuit share link; SHA-256 of extracted file matched |
| Abrupt home failure during that get | SIGKILL of the provider's selected active home at 1,051,616 verified bytes (2.370 s); same caller/operation succeeded without retry/error via the other relay, 12.733 s total including failure detection/recovery |
| EIM/APDF after removing the direct block and emitting route invalidations | Both real daemons reported authenticated direct paths with warm backup relay; 16.994 s in the recorded run (includes owner hooks/backoff), not an instantaneous-upgrade claim |
| ADM/APDF ↔ ADM/APDF | Real-daemon relay get succeeded; 273,000 verified bytes, 84 ms whole CLI operation |
| APDM/APDF ↔ APDM/APDF | Real-daemon relay get succeeded; 273,000 verified bytes, 75 ms whole CLI operation |
| Native Linux conntrack NAT pair | Real-daemon relay get succeeded; 273,000 verified bytes, 72 ms whole CLI operation; no RFC NAT-class promise for the kernel allocator |
| All forwarded UDP blocked; fresh recipient daemon/new CID | Get failed `unavailable`, with the explicit online/UDP-reachability action; it did not silently satisfy the request from previously held bytes |

All four daemon mapping cases connected successfully. APDM is not advertised as
direct-hole-punch success. Loopback assembly tests separately establish crossed
attempt convergence, active-stream failover on the **same** inner QUIC connection,
exact endpoint authorization/negative targets, private candidate non-disclosure,
LAN/loopback exclusion from public signed snapshots, two-observer proof aggregation
and mapped-address publication before relay withdrawal, private/isolated shutdown
and real D12 hook registration. An actual route event
returns the physical-rebind limitation rather than a false successful rebind.

A race-instrumented loopback assembly run measured the first five-byte application
echo at 31.65 ms, circuit/inner-handshake setup at 23.71 ms, and the inner relay RTT
at 2.38 ms. Its acceptance check bounds the first echo by setup + relay RTT + a
50 ms scheduler/instrumentation allowance; this is a fast no-added-stagger check,
not a production latency SLO or a zero-noise RTT equality assertion.

The cold-route race test has a 750 ms deadline against a 1 s configured stagger:
the relay starts immediately alongside direct setup; it cannot wait behind that
stagger or a dead direct timeout. Disco control negotiation is background work,
not an extra round trip before the first data stream. Whole-CLI/get-progress times
above are **not wire TTFB measurements** (progress is throttled and Bao verification
is chunked); no exact RTT-only numeric performance claim is inferred from them.
No default public relay fleet, production abuse/admission rollout, physical UDP
socket replacement, cross-family inner multipath, or public field NAT success
rate is established by this evidence.

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

### Remaining 0017 acceptance (2026-10-22)

Added real namespace/nftables scenarios on baseline `ae66acc` plus these tests:

| Pair | Both direct after Open | Transfer check |
|---|---:|---|
| Public IPv4 ↔ EIM/APDF | 39.1 ms | 4 MiB byte-verified, same inner connection |
| IPv6 ↔ IPv6, no NAT (`2001:db8:1::/64`) | 11.5 ms | 4 MiB byte-verified, same inner connection |
| miniupnpd UPnP-mapped ↔ EIM/APDF | 43.0 ms | 4 MiB byte-verified, same inner connection |

The mapped case uses **real miniupnpd 2.2.3**, its nftables backend, and
`p2p/portmap` UPnP discovery/lease acquisition. The test checks the daemon's actual
DNAT rule, feeds the lease through `disco.FromMapping` with expiry preserved, and
uses the granted external endpoint. `11.0.0.0/24` is confined to the isolated
namespace (no Internet uplink), because miniupnpd rejects reserved WAN addresses.
IPv6 runs on real veth/bridge interfaces with DAD disabled only in the test peers.
Together with the earlier EIM/EIF and EIM/APDF cases, these cover the requested
baseline classes; they are not an exhaustive field/NAT implementation matrix.

The opt-in bulk test transferred **1 GiB (1,073,741,824 bytes)** across EIM/EIF ↔
EIM/APDF in **35.37 s**. It performed a standalone **200 ms interface flap**
(progress **163,840 → 4,636,672 bytes** after recovery), then changed both inside
and outside addresses as in a Wi-Fi → hotspot move during the same transfer.
Direct recovery after the address replacement took **1.030 s**. All 65,536
16 KiB chunks were byte-compared; **the same inner QUIC connection and application
stream stayed open**, without replaying or resetting the verified progress counter.
The fixture restores the default route after link-up, as an OS network manager
would: without that route, relay progress continued but direct probing correctly
could not recover. The stable Unix-stream relay leg caveat above still applies;
real circuit/daemon failover belongs to 0020. This supersedes the earlier statement
that the 1 GB / IPv6 / mapped acceptance scenarios were unverified. Physical
socket replacement, constrained MTU and long field matrices remain unverified.

Reproduce the added acceptance (miniupnpd is required for the mapped case; without
it that test reports a skip, not a pass):

```
NU_MINIUPNPD=/path/to/miniupnpd go test -tags natlab ./p2p/disco -run 'TestNATLab(PublicEIM|IPv6|MappedMiniUPnPd)$' -v -count=1
NU_SLOW=1 go test -tags natlab ./p2p/disco -run '^TestNATLabDiscoGiB$' -v -count=1 -timeout=5m
```

`slow.Skip` and the `natlab` build tag both gate the bulk test; it does not run in
the default suite. `natlab.IsolateTimeout` supplies an explicit four-minute child
deadline for this test, leaving the normal 45-second fixture deadline unchanged.

### Independent security review request / review plan (0017 and 0020)

**Requested; pending independent review.** The implementation author is not the
independent reviewer. A networking/security reviewer should review the following
before public enablement; record the reviewer, reviewed commit, findings and
follow-up commits here. Passing the smoke/soak tests is not security approval.

- **Contract and implementation:** D11's “Path v1 control and probe security
  contract”, `p2p/disco/{wire,service,candidates}.go`, the magicsock demux/path-set
  boundary, and the eventual 0020 socket-owner `Authorize`/candidate-disclosure
  policy. Review the library and the assembly together, not just MAC primitives.
- **Threat model:** untrusted authenticated peers may assert victim endpoints;
  UDP sources can be forged; a relay can observe, delay, replay, reorder or drop
  frames but must not authenticate as an endpoint. Include a compromised trusted
  peer, concurrent/crossed circuit attempts, network changes and control-stream
  reopening on the same TLS connection. The relay must not grant application
  membership, bypass Host gates, or turn metadata assertions into peer identity.
- **Authorization:** exact asserted endpoints plus independent owner policy;
  sensitive/private/loopback/infrastructure destinations, interface scope and
  directed broadcasts; PCP PEER restrictions and lease expiry; no implicit
  permission from pong observations or incoming source addresses. Verify public
  peers cannot use a permissive callback as a UDP scanner/reflection service.
- **Cryptography/replay/state:** inner rather than outer TLS exporter, sorted
  identity context, independent hello nonces, directional keys, constant-time
  MAC check, sliding replay window, challenge/destination/expiry matching,
  counter/generation exhaustion, duplicate streams, revocation and key teardown.
- **Abuse and lifecycle:** combined ping/pong peer/socket rate budgets, no size
  amplification or unsolicited errors, bounded decoding/queues/challenges,
  candidate replacement/expiry, rejection before state growth, cancel/join and
  slow-reader behavior, path-proof revocation, and fallback without weakening
  QUIC authentication. Read security negatives and fuzz seeds as evidence, then
  devise independent adversarial cases (including policy bypass and replay after
  teardown). Prediction/spraying needs a separately negotiated reviewed contract.
- **Exit:** resolve high-severity findings before untrusted-peer activation;
  explicitly record residual trust assumptions and limits. 0020 must not treat
  this pending request or the mandatory callback alone as permission to enable
  arbitrary-peer probing.

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
| E6, placement sync | same | 3 routed private members, `replicas: 2`: `ready` once every blob has two confirmed copies (`copies.below` 0, the writer's `full` report counts); each member holds the structure and exactly the blobs it owns; `held` wait (mode `owned`) names holders, non-member holder fails |
| R3 (E1/W1) | same | a wait on T keeps T after the head moves and reports `superseded_by`, while the new head becomes ready on its own |
| R4, R5, R7, R8, E8 | `cluster.TestReadyMatrix` (fake clock) | other-epoch/policy report → `missing: epoch`; clock past `evidence.fresh` → `unknown`, not degraded; k-of-n: a stale owner's blobs confirmed by a `full` holder → ready with the owner `missing: stale`; incomplete surplus holder → in neither `confirmed` nor `extra`; complete non-owners only → not ready, `extra` shown; fresh contradiction after ready → `degraded` |
| Collection floor placement | `collection_test.TestCollectionFloor` | `replicas: 2` on 3 members: each item's two floor owners (rendezvous on the item-root multihash, vectors `items`) hold it whole with src `floor`; `where` names the owners, the index counts the floor copies; `ready` at the merged root with `copies.unit: item`; a fourth member moves items to itself and a previous owner keeps its copy as `surplus` |
| R6 (E5) | `TestPlacementReadiness`, `TestReadyMatrix` | `replicas: 4` on 3 members and `spread: zone` without zones fail a ready wait fast with the shortfall; the team default warns instead of sharing failure domains silently |
| Heartbeat reports | `cluster.TestReportDelta` | deltas apply in sequence, a missed heartbeat forces a full pull, removals propagate |
| R15 (S5) | `replica.TestFirstHeldProtection` | never-ready dataset, five head changes with unavailable content: the last fully held view stays pinned, two heads staged, later heads observed only |
| R17 (S7) | `replica.TestPressurePauses` | store limit during an update: `paused: insufficient_storage` with bytes needed, protected pin and content intact |
| One-node `k=1` | `cluster_test.TestLocalReady`, `cmd/nu.TestDatasetWaitCLI` | local dataset ready at `required: 1`, epoch `local`; CLI `update`/`wait`, timeout exit 6 with the snapshot |
| `release.auto=false` | by construction | nothing in 0046 unpins held or ready versions; only never-held staged heads are dropped |

Not yet covered: E8 under a real partition in nutest (the fake-clock table covers the
rule), the Linux isolated lab with placement traffic, and floor-surplus release (0048).
Network tests retry joins (each attempt with its own dial deadline) and wait on
conditions with generous deadlines, so a loaded shared machine slows them rather than
failing them.

## Version history and archives (0047)

Scaled to the v0-era test policy: a few fast tests per acceptance bullet (Linux only).

| Bullet / rule | Test | Shows |
|---|---|---|
| One-copy archive preset, two versions, exact old bytes without PDS or other nodes | `node.TestOneNodeArchive`, `cmd/nu.TestCLIArchive` (daemonless) | `--archive` writes the default schedule with `replicas: 1`; `--source` imports the first version; `update` re-adds; `cat`/`get -o` of `<ds>@<tid|label|time>` return the old bytes; `/c/<ds>@<label>/<path>` redirects to `/tree/<cid>/<path>` |
| R9 (E6), E8 archive-wide | `node.TestOneNodeArchive`, `cluster_test.TestClusterArchive` | one node: content and archive state `ready`, `required: 1`, one confirmation; two members (`replicas: 1`): `wait --condition archive-ready` needs one confirmed copy of every blob of the live set (`required: 1`, `copies.below` 0) |
| New incomplete version vs established availability (E8) | `cluster_test.TestClusterArchive` | a head whose content exists nowhere makes the archive not ready while the older version stays `retained-complete`; reading the new version is `unavailable`; the old one lists through placement |
| Rollback as a new head | `TestOneNodeArchive`, `TestClusterArchive` | new head and version with `restored_from`; the restored version's identity and label unchanged; metadata and content are separate results |
| Incomplete/never-replicated/pruned never claim retrieval; skipped versions | `node.TestArchiveRetentionAvailability` | a head with unheld content is `observed`, superseded outside the live set it is `skipped`; a pruned version is `pruned`, reads (by TID or by a time selecting it) and rollback fail `unavailable` naming it, never a neighbour |
| Shared references prevent unsafe deletion; dry-run | same | the dry run lists the candidate with free and shared bytes and changes nothing; after prune and GC the other dataset and the live version still read the shared file; networked prune is dry-run only (0047; gated by owner confirmation since 0048) |
| Labels and held copies survive budget pressure | `node.TestArchiveBudgetPressure` | an import over `store.limit` fails `insufficient_storage`; after GC the labeled and held versions are `retained-complete` and read |
| Explicit missing-version reacquisition | `TestArchiveRetentionAvailability` | labeling pruned content fails without `--reacquire`; with it the version is live and still not retrievable until its content is complete again |
| Immutable identity vs annotations, collisions, compaction | `cluster.TestVersionMerge` | an annotation copy merges by (hlc, signer); a later self-authored redefinition of the same TID is a reported conflict, never merged; a winning tombstone compacts; label syntax excludes TIDs and times |
| Retention schedule, lookup without fallback | `cluster.TestRetainAndSelect` | GFS buckets from the newest version, labels and head always live, `max_bytes` drops the oldest unprotected and reports protected excess; deterministic live-set identity; a time before history is an error; a duplicated label is ambiguous |
| Crash consistency | `TestOneNodeArchive` (restart) | head and version are one signed commit; history, labels and availability survive a restart |

Not covered: macOS/APFS crash and sleep tests (see 0044 below for Unicode/case),
process kill between the
import and the head write (the import is unpinned and falls to GC after the grace),
authenticated import-manifest export (needs 0042's manifests) and historical identity
evidence for external sources (0045, 0054).

## Repair, handoff, audits and release (0048)

Scaled to the v0-era test policy (Linux, 2026-10-22): nutest scenarios on loopback QUIC
with partitions (connection gating), kill/restart, store limits as disk pressure and
shortened settings (`node.ClusterDefaults`, `node.EvidenceFresh`: fresh 2 s, settle
0.3–2 s, down/up 1 s, interval 100 ms) in place of a fake clock; the rule logic itself
has a clock-free table test. Commands: `go test ./cluster -run
'TestReleaseRules|TestRepairAfterLoss|TestSafeRelease|TestDrainRemove|TestAuditScrub|TestClusterArchive'`
(the four network tests passed three consecutive runs).

| Row / rule | Test | Shows |
|---|---|---|
| R10 (S2) | `cluster_test.TestSafeRelease` | after a head update with one owner partitioned off from the other two, the previous head stays pinned on all three members for `evidence.fresh` + 3 s; after healing it is released everywhere |
| R11 (S2) | `TestSafeRelease`, `cluster.TestReleaseRules` | the dry run during the partition lists the old head as not releasable with the partitioned owner in `shortfall` (`stale`/`absent`); table: one stale owner of `k=2` blocks and is named |
| R12 (S3) | `TestSafeRelease` | a capacity edit (epoch change) right after the new head is ready: the dry run is `deferred: unsettled` and the old head stays pinned; it is released once the epoch settled |
| R13 (S3) | `cluster.TestReleaseRules` | an owner reporting another epoch defers every release (`epoch_disagreement`) and does not count; also held-back records and an owner whose record changed within settle |
| R14 (S4) | `TestSafeRelease` | b becomes a second admin (`key new` / `key grant`); a and b write heads while partitioned; after healing both see the other head in `displaced`, the dry run is `deferred: concurrent_heads`, and the previous head stays pinned on every member |
| R16 (S6) | `cluster_test.TestDrainRemove` | `rm` of an active owner is refused (drain first); a drained member is not removable while an owner of its blobs is partitioned (`failed`, `unconfirmed` names the owner) and keeps its view pinned; after healing it releases its copies once new owners confirm them and `rm` succeeds unforced; `--force` during another partition removes and reports `forced` with the unconfirmed content |
| R18 (E9) | `cluster_test.TestAuditScrub` | `replicas: all` with one blob that exists nowhere: a member holding the rest passes sampled verified chunk-group challenges, stays out of `confirmed`, the dataset is not ready, and cluster status shows its audits separately |
| R20 (E8), lost node with authority | `cluster_test.TestRepairAfterLoss` | an authority restarts (`cluster.repair auto`) after a storage member died; after `down_after` it writes `{unavailable, auto}`; with no free capacity on survivors the dataset is `degraded` (not unknown) with `repair: in_progress` and a `degraded` alert, then `ready` with `copies.min ≥ 2` without the dead member once capacity frees; when the member returns it is `active` again after `up_after` |
| No repair authority | same | the member dies with repair off everywhere: `unknown` (never `degraded`), `repair: unavailable`, `no_repair_authority` alerts in the dataset and cluster status, and the member record stays `active` past `down_after` |
| S2 surplus release | same | the writer drops its explicit pin; `gc --dry-run` lists its non-owned blobs as `surplus`, releasable once owners confirm; `gc` removes exactly the blobs it does not own |
| Scrub → repair | `TestAuditScrub` | a flipped byte in a stored blob file is found by `audit --full`, dropped (`scrub_failed` alert) and fetched again from members |
| Networked prune gate (S1) | `cluster_test.TestClusterArchive` | prune with an incomplete live version fails `failed` with the shortfall |
| CLI, one-node release | `cmd/nu.TestDatasetGCCLI` (daemonless) | `dataset gc --dry-run` lists the previous head as a releasable version (one member: nothing to settle) and changes nothing; `gc` unpins it and the current head still reads |

Not covered: crash (kill) between a release decision and the store removal (removal is
one transaction under the GC lock; the in-memory settle restarts after any restart, so a
crash only delays release), floor-surplus release for collections (surplus items stay
held), a Linux isolated-lab (natlab) run of repair traffic, Prometheus metrics (none in
this build; status JSON carries the alerts), byzantine audit evasion (a member can relay
challenge answers from another member: audits prove serving, not storage).

## Distribution, upgrades and recovery (0044)

Scaled to the v0-era test policy (Linux, 2026-10-22). Rules are
[B1–B8, U1–U6, I1–I5](operations.md#b-backup-export-and-recovery).

| Rule / scenario | Evidence | Shows |
|---|---|---|
| Clean-machine restore from a complete export + backed-up authority (B1–B6) | `node.TestCleanMachineRestore` | a one-node archive with two versions and a 9 MiB blob (stored beside the CAR) is exported and backed up; the repo is deleted; restore verifies the signed state under the same PeerId, history is back but content unreadable until import; import verifies every object, restores the pins; both versions read byte-exact; a truncated bundle pins nothing; restoring over another node key is `conflict` |
| Clean-machine restore from surviving members (B6) | `cluster_test.TestRestoreFromSurvivors` | the founding member (root + admin keys) of a two-member archive cluster is wiped and restored from its authority backup only; it rejoins as the same member, re-acquires both retained versions from the survivor, and its admin key still writes a head the survivor accepts |
| Interrupted upgrade (U2/U3) | `store.TestInterruptedUpgrade` | a store one schema behind is opened by a child process SIGKILLed inside the last migration's transaction; the schema stays at the previous version, the next open resumes and completes it (derived columns and usage counters correct) |
| Newer formats refused untouched (U4/U5) | `store.TestInterruptedUpgrade`, `repo.TestNewerLayoutRefused`; CLI by hand | newer `user_version` → `ErrNewerSchema`, version unchanged; newer layout → `ErrNewerLayout`; `nu status` exits 4 (`not_supported`) with "run the newer release; downgrades are not supported" |
| Filenames, Unicode, case, symlinks, modes (data.md Platforms, X1–X6) | `node.TestFilenameRoundTrip`, `extract.TestExtractSafety` | Linux (ext4/tmpfs): NFC and NFD spellings, `Readme.md`/`README.md`, 254-byte and emoji names round-trip byte-exact through add and `get -o`; `0750` → `0755`, `0600` → `0644`; the symlink is recorded, reported with its target and not created; a FIFO is skipped and reported on add; a non-UTF-8 name fails the add naming the file. On a folding filesystem the second spelling is reported naming the entry it maps onto (asserted at run time; exercised by the macOS CI job) |
| CLI round trip and permissions | by hand, `nu repo export/backup/restore/import/unpin` | backup tar and its entries `0600`, bundle files `0600`; restore into a fresh `NU_PATH` then import; `nu repo unpin` removes a pin of any source |
| Release build (I1) | `scripts/release.sh build/package` by hand | static linux/amd64 binary; tarballs with fixed order/owner/mtime and `0644`/`0755` modes; packaging twice gives identical SHA-256 |
| Install, upgrade, pre-upgrade backup (I4/I5) | `dist/install.sh` by hand on this Linux workstation (systemd user unit), then removed | install from a tarball (checksum verified), daemon up under `systemctl --user`; upgrade v0.0.1 → v0.0.2 stops the unit, writes a `0600` authority backup, swaps the binary, restarts; held content still served; a corrupted tarball is refused by its checksum; `--no-service` install/upgrade twice in a prefix |

Bugs found: the installer's readiness probe used `nu status`, which falls back to an
in-process node and took the repo lock from the starting daemon; it now probes with
`nu operations` (daemon only).

**Human or CI only (not run here):**

- macOS: native runs of the arm64/amd64/universal binaries beyond the release workflow's
  smoke step; the LaunchAgent install/upgrade path of `install.sh`; `plutil -lint` (CI);
  APFS case/normalization extraction (the macOS `go test` job runs
  `TestFilenameRoundTrip`/`TestExtractSafety`; its results are not recorded here yet).
- Isolated/private-mode egress on macOS: the netns lab results (`-tags natlab`) are Linux
  evidence only and authorize no macOS isolation claim.
- APFS crash consistency and sleep/wake (power cut during migration and during sync on a
  laptop; lid close while serving). Linux kill tests do not authorize macOS claims.
- Signing and notarization: needs an Apple Developer ID certificate and notary
  credentials as repository secrets (I2); until then macOS binaries ship unsigned and
  Gatekeeper will warn on browser-downloaded copies.
- `--system` install on a Linux server as root (user creation, `/var/lib/nu`, unit
  hardening under real workloads; `systemd-analyze verify` passes apart from the absent
  binary).
- Security review of backup/restore (archive parsing limits, plaintext key handling,
  restore preconditions) and of the units' hardening, per the independent review plan.
- Re-genesis (B8) as an end-to-end drill; there is no `nu cluster leave`, so it is a manual
  procedure today.

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

## Download tuning (0071, 2026-10-21–22)

**Controlled transport-relative and capped-provider targets were attained, but not
consistently under shared-storage stalls. The original 10 GB end-to-end scenario
still falls short of the single-peer throughput target.** These are local results,
not a field-network or dedicated-disk guarantee. The 0071 backlog file was absent
from this worktree; the task handoff supplied its acceptance criteria.

Machine: AMD EPYC 9684X, 192 visible logical CPUs/default GOMAXPROCS, Linux
5.15.0-176-generic, Go 1.26.2 linux/amd64, quic-go v0.64.1. Unpinned shared host,
ext4 on `/dev/mapper/ubuntu--vg-ubuntu--lv`, about 96% full (84 GiB available at
start). No sysctls, QUIC windows, dependencies or hash/proof formats changed.
quic-go warned that the UDP receive buffer could only grow to 416 KiB, not its
requested 7168 KiB. No external network traffic was used.

### Method and measurements

`nutest/download_test.go` supplies opt-in (`slow.Skip`, `NU_SLOW=1`) benchmarks:

- `BenchmarkPlainQUIC`: two actual quic-go loopback endpoints, one stream,
  TLS/encryption, 2 GiB to `io.Discard`, without Nu, disk or Bao. Both Nu's current
  receive windows and upstream default windows are measured; connection setup is
  untimed. This is the reference ceiling, not a hash/memory-copy microbenchmark.
- `BenchmarkDownload`: real Nu hosts, `/nu/have/1`, scheduler, `/nu/bytes/1`, Bao
  verification, partial files, SQLite checkpoints, fsync and final commit. Each
  iteration uses a fresh destination. Default 2 GiB input is a **sparse zero file**
  registered nocopy, avoiding source-disk read bandwidth and size-proportional Go
  heap. QUIC does not compress it. Source creation, connection setup and an
  independent final BLAKE3 digest/length check are outside the benchmark timer.
- Capped case: each of three providers has its **own shared-across-streams**
  40,000,000 B/s write pacer (64 KiB maximum write, no accumulated idle credit).
  The third flips a payload byte. Its rejection is asserted, as is the final
  digest. The denominator is **80 MB/s**, not the uncapped one-peer loopback rate.
  Throughput ratios are reported, not asserted against noisy CI timing.
- CPU, block and mutex profiles were collected with `go test` and inspected with
  `go tool pprof`. Profiles include untimed setup/final verification too; scoped
  call stacks distinguish those from the transfer. Profiled and unprofiled
  timings are identified below; do not treat single samples as precise speedups.

All MB/s below are decimal. Final repeated measurements use three `-benchtime=1x`
runs (not three downloads into an already populated destination):

| Measurement | Before / control | Final result |
|---|---|---|
| Plain QUIC, Nu receive windows, 2 GiB | No transport change | 336.02 / 383.71 / 326.29 MB/s |
| Plain QUIC, upstream windows, 2 GiB | No transport change | 324.50 / 326.49 / 322.88 MB/s |
| Controlled single peer, 2 GiB | 227.94 MB/s, profiled before production edits | 252.31 / 278.04 / **45.85** MB/s, unprofiled; median **252.31** |
| Controlled single peer, 10 GB | 161.76 MB/s after read-ahead/checkpoint overlap, before write batching, profiled | **281.34 MB/s**, 35.54 s, profiled after write batching |
| Three capped peers, one corrupt, 2 GiB | Initial 512 MiB profiled control: 73.74 MB/s, 0.922 of good caps (different size) | **34.80 / 79.16 / 79.21 MB/s**; 0.435 / **0.990 / 0.990** of good caps |
| Original 10 GB node fetch + final digest | Historical **73.72 MB/s** | **107.73 MB/s**, 92.83 s (1.46× historical) |
| Original 10 GB, three uncapped peers, one corrupt, + digest | Historical **47.27 MB/s** | **85.83 MB/s**, 116.51 s (1.82× historical; not a capped-link sum test) |

Even using the **fastest** plain-QUIC sample, the lower single-peer target is
`min(5 × 73.7, 383.71 / 2) = 191.86 MB/s`: the controlled median and 10 GB sample
exceed it. Two of three final capped runs exceed 0.8 of the honest caps. However,
one single and the immediately following capped run had large stalls and failed
those thresholds; they are retained above rather than discarded. No competing
host workload or disk latency telemetry was captured, so attributing those
outliers specifically to another user is not established.

The original `TestHardening10GB` was rerun unchanged, including physical source
files, full nodes, 128 MiB soft Go limit, 5 ms `runtime.ReadMemStats` polling and a
final digest **inside** the timer. It passed in 433.78 s. Download sampled peak Go
heap was **24.82 MB single / 28.99 MB multi** (not RSS/page cache); local streaming
memory assertions passed. Four corrupt responses were served and rejected.
That scenario remains below 191.86 MB/s; sparse-source benchmarks must not be
presented as a 5× improvement to it. Storage cost, instrumentation and final
readback are included there but not isolated by this campaign. An earlier
intermediate run was 72.20 / 105.38 MB/s before write batching, further evidence
that shared-host timing is variable.

### Findings and changes

1. **Checkpoint barrier:** every worker acquired the same mutex after every
   1 MiB request, even while another worker held it across fsync. The initial
   2 GiB mutex profile attributed **4.33 s aggregate wait** to that checkpoint
   closure. Now one worker checkpoints while others proceed, with backpressure
   after one more checkpoint's worth of completed work. The default cadence
   remains **16 MiB**, and final Commit still waits for all workers and syncs.
   This permits bounded overlap (at most two checkpoint intervals plus in-flight
   chunks), not unlimited uncheckpointed work. Scheduler chunk size, four workers
   per peer, corruption dropping and endgame cancellation are unchanged.
2. **Small positional I/O:** Bao's traversal issued one read/write per 16 KiB
   group and per 64-byte parent. Server request-local read-ahead uses 64 KiB for
   data and 4 KiB for parents. The client similarly coalesces already-verified
   writes. It publishes group bits **only after both data and proof writes**;
   corrupt/truncated responses still flush their verified prefix. No unchecked
   data is stored, no full blob/outboard is cached, and SHA-256's 8 MiB whole-object
   path is unchanged. Additional buffers are 68 KiB per active request on each
   side, independent of blob size; byte-identical proof verification is retained.
   The focused 10 GB decoder CPU profile's syscall samples fell **26.92 → 20.29 s**,
   while group-hashing CPU stayed **6.49 → 6.42 s**. Checkpoint-overlap wait in the
   block profile fell **77.36 s → 0.115 s aggregate** after write batching. These
   support small I/O/checkpointing as bottlenecks, not BLAKE3 speed.
3. **Checkpoint correctness:** serialize store Flush snapshots so an older map
   cannot overwrite a newer one; restore dirty state after sync/SQLite failures.
   Fetch now returns checkpoint errors as local errors rather than losing them
   or calling them unavailable providers. Data/proofs are still synced before
   SQLite advertises held groups; buffering does not weaken resume durability.
4. **Not changed:** QUIC window enlargement was not supported by the reference
   measurements (upstream defaults were not faster). A 10 GB trial with a 64 MiB
   checkpoint cadence achieved only **121.38 MB/s**, versus 161.76 at 16 MiB in
   the preceding intermediate run; it was not adopted. QUIC packet/syscall,
   runtime/GC and disk costs remain substantial; hash throughput alone is not a
   realistic encrypted-network/download ceiling.

Reproduce from the worktree root (default benchmarks skip without `NU_SLOW=1`):

```sh
mkdir -p .hardening/download
NU_SLOW=1 go test ./nutest -run '^$' \
  -bench 'Benchmark(Download|PlainQUIC)' -benchtime=1x -count=3 -timeout=15m
NU_SLOW=1 NU_DOWNLOAD_BYTES=10000000000 go test ./nutest -run '^$' \
  -bench 'BenchmarkDownload/single$' -benchtime=1x -count=1 -timeout=10m \
  -cpuprofile=.hardening/download/download.cpu \
  -blockprofile=.hardening/download/download.block \
  -mutexprofile=.hardening/download/download.mutex \
  -o .hardening/download/nutest.test
go tool pprof -top .hardening/download/download.cpu
go tool pprof -top .hardening/download/download.block
go tool pprof -top .hardening/download/download.mutex
NU_SLOW=1 go test ./nutest -run '^TestHardening10GB$' -count=1 -v -timeout=25m
# Optional checkpoint experiment, without changing production defaults:
# NU_DOWNLOAD_FLUSH_BYTES=67108864 NU_SLOW=1 ...
```

Local raw logs/profiles are under ignored `.hardening/download/`: `single-before.*`,
`single-after.*`, `after.log` (reference and intermediate repeats), `final.*`
(intermediate bounded-overlap profiles), `10gb.*`, `10gb-flush64.log`,
`10gb-batched.*`, `batched-final.log`, and `legacy-10gb-batched.log`.
Fast tests cover cache seeking/EOF, verified-prefix preservation after corruption
and truncation, delayed publication on write failure, and retry/concurrent durable
checkpoints. Pre-commit `go vet ./... && staticcheck ./... && go test ./... &&
./scripts/depguard.sh` passed, as did focused race tests for partial checkpoints,
read-ahead, resume and multi-peer corruption recovery. Remaining gaps: consistent
throughput on busy/full storage, the original 10 GB single-peer target, physical independent providers,
WAN/high-BDP tuning, and remote/native-platform execution. No backlog edits.

### 0071 stall investigation: destination sync latency (2026-10-22)

**Reproduced and isolated, not fixed in production.** The intermittent low rates
are reproducible without Nu: a single-goroutine sequential-file-write + sync
control on the same filesystem also falls to approximately 50 MB/s. Nu's bounded
checkpoint backpressure exposes that destination storage latency; there is no
positive evidence of a scheduler/endgame deadlock or a two-minute request timeout
in these runs. It would be incorrect to claim a production fix by loosening the
resume bound, skipping sync, or relabeling RAM-backed results as durable-disk
performance. No production code, checkpoint cadence, timeout or memory limit was
changed in this investigation. A speculative `fdatasync` replacement was tested
and **discarded**: five single-peer trials still included a 39.09 MB/s result.

Same EPYC/Go/kernel as above; base `a64a277` after merging master. Destination
`/tmp` is on the root ext4/LVM volume, **KINGSTON SNV2S2000G (`nvme2n1`)**, 96%
full, approximately 77 GiB free at the end. The exact lower-level cause of the
I/O latency (device cache/GC, filesystem behavior, or competing workload) is not
established; this is not a hardware-health diagnosis. The capture identifies the
blocking I/O layer, rather than assuming the pauses came from an unrelated user.

#### Repeated results (2 GiB each; decimal MB/s)

Each row has **10 independent fresh-destination runs**, `-benchtime=1x -count=10`.
CPU count, endpoints, verification, caps and code are unchanged. Block/mutex
profiling and request diagnostics were enabled for both disk and RAM downloads.
The single-peer threshold is the previously measured conservative 191.86 MB/s;
the capped case threshold is 64 MB/s, 0.8 of two honest 40 MB/s providers. One
additional 40 MB/s provider corrupts its responses and is dropped in every run.

| Measurement set | Minimum | Median | Maximum | Below target |
|---|---:|---:|---:|---:|
| Initial disk, single | 44.93 | 259.54 | 270.16 | 3/10 |
| Initial disk, capped three/one corrupt | 47.80 | 78.72 | 79.33 | 3/10 |
| Final disk, single (enhanced watchdog) | 123.54 | 272.62 | 296.39 | 1/10 |
| Final disk, capped three/one corrupt | 44.52 | 79.10 | 79.26 | 2/10 |
| RAM isolation control, single | 257.80 | 269.52 | 281.42 | 0/10 |
| RAM isolation control, capped three/one corrupt | 78.39 | 79.09 | 79.44 | 0/10 |
| Storage-only sequential write + sync | 49.84 | 478.75 | 538.89 | Not a network target |

Final disk capped min/median/max as fractions of the honest sum:
**0.557 / 0.989 / 0.991**; RAM capped **0.980 / 0.989 / 0.993**. All download
runs passed their independent final size/digest check and corrupt-peer assertion.
Across the two disk measurement sets, **9/40 downloads missed the threshold**.
This is not a universal one-in-three failure probability; the bad rates cluster
in time, and the small sample is from one shared storage device.

The RAM control used an 8 GiB tmpfs mounted **only inside an unprivileged private
mount namespace** at `.hardening/stall/ram-tmp`, with `TMPDIR` set there. It did
not modify host mounts or sysctls. The mount and data disappeared on namespace
exit. Both final-disk and RAM test binaries have identical SHA-256
`2c33301ef2b9f51ff9c04228fc4419a5cc5cdbdf7cf337eef969c3bbe491e1fb`.
**Tmpfs sync is not persistent storage or power-loss evidence**; this comparison
isolates Nu/QUIC scheduling from the physical destination device. It is not a
workaround to deploy or a replacement for the failed physical-disk measurements.

#### Captured cause and excluded candidates

- The initial idle watchdog captured a single-peer pause at 2,084,569,088 bytes,
  with **no active requests**. One range worker was in `syscall.Fsync` from
  `store.(*Partial).Flush` (data file); the other three waited on the checkpoint
  completion channel. No worker held `Partial.mu` while waiting for a peer.
- Many slow runs continued completing a few chunks every second, so an idle-only
  `>1s` watchdog missed them. The enhanced watchdog also triggers after two
  consecutive seconds below the scenario's throughput threshold. In the final
  disk set it captured **34 goroutine dumps across all three slow runs; 33 had a
  worker in `fsync`**. It captured **zero** dumps in the 20 RAM-control downloads.
- All final-disk single-peer runs performed **2,049 requests with zero resets**
  (have + 2,048 ranges). Their maximum request duration was 0.534 s and maximum
  individual stream-read wait 0.086 s. Slow capped runs had maximum requests
  0.234/0.664 s and stream-read waits 0.053/0.040 s. Expected corrupt/endgame
  resets occurred, but there was no 30 s or 120 s retry delay. Single-peer runs
  cannot take the cross-peer endgame duplicate path yet reproduced the slowdown.
- The final disk block profile attributes **162.23 s aggregate worker wait** to
  the checkpoint completion channel. The byte-identical RAM-control binary has
  **no sampled wait at that channel** (0.172 ms aggregate elsewhere in the
  checkpoint closure). This is storage backpressure, not a lock-order cycle.
- A separate three-run `strace -f -tt -T -y -e trace=fsync,fdatasync` capture
  measured **125.27 s in 372 partial-data syncs**, 18.20 s in 369 outboard syncs,
  and only 0.13 s in 12 SQLite WAL syncs. Maximum data-file sync was **2.551 s**.
  These syscall-traced runs are diagnostic, **not performance measurements**:
  ptrace materially slows the whole process. Final, non-ptrace `iostat -dx -t 1`
  independently recorded `nvme2n1` write await up to **642.85 ms**, queue depth
  **107.72** at that sample, and about 38 MiB/s completed writes.
- `BenchmarkCheckpointIO` removes the entire Nu stack: one goroutine, no locks,
  network, SQLite or hashing, sequential 64 KiB writes and `File.Sync` every
  16 MiB. Two of ten runs fell to **61.17 and 49.84 MB/s**, with **90.9% / 91.5%**
  of elapsed time in sync and maximum individual syncs **1.403 / 1.658 s**.
  Seven runs were 472–539 MB/s, and one was 229.80 MB/s. Thus neither QUIC flow
  control nor Nu's verified-write path is required to reproduce this behavior.

#### Harness changes and reproduction

`NU_DOWNLOAD_TRACE` now enables per-request elapsed time/received-byte/read-wait
records and one-second progress samples in JSONL. Each run has its own directory;
up to 12 low-progress/low-throughput goroutine snapshots plus a first-stall block
profile are saved. The block snapshot needs `-blockprofile` to enable sampling.
Tracing is test-only, optional, bounded in live memory, and does not change the
fetcher. Traces close and their watchdog joins even on benchmark failure.

```sh
mkdir -p .hardening/stall
NU_SLOW=1 NU_DOWNLOAD_TRACE="$PWD/.hardening/stall/disk" \
  go test ./nutest -run '^$' -bench '^BenchmarkDownload$' \
  -benchtime=1x -count=10 -timeout=30m \
  -blockprofile=.hardening/stall/disk.block \
  -mutexprofile=.hardening/stall/disk.mutex -o .hardening/stall/disk.test
NU_SLOW=1 go test ./nutest -run '^$' -bench '^BenchmarkCheckpointIO$' \
  -benchtime=1x -count=10 -timeout=20m
# Run iostat -dx -t 1 alongside the disk set; stop it when the set finishes.
go tool pprof -list='download.func3' .hardening/stall/disk.block
# Each trace directory contains events.jsonl, and, on a stall,
# stall-00.txt ... stall-11.txt and stall.block.

# Optional Linux isolation control, NOT a persistent-storage benchmark:
mkdir -p .hardening/stall/ram-tmp
unshare --user --map-root-user --mount sh -c '
  mount -t tmpfs -o size=8G tmpfs "$PWD/.hardening/stall/ram-tmp" &&
  TMPDIR="$PWD/.hardening/stall/ram-tmp" NU_SLOW=1 \
  NU_DOWNLOAD_TRACE="$PWD/.hardening/stall/ram" \
  go test ./nutest -run "^$" -bench "^BenchmarkDownload$" \
    -benchtime=1x -count=10 -timeout=20m \
    -blockprofile=.hardening/stall/ram.block \
    -mutexprofile=.hardening/stall/ram.mutex -o .hardening/stall/ram.test'
```

Local raw evidence is under ignored `.hardening/stall/`: `before.log`, `before/`,
`before.{block,mutex}`, `disk-final.log`, `disk-final/`, `disk-final.{block,mutex}`,
`iostat-final.log`, `ram.log`, `ram/`, `ram.{block,mutex}`, `checkpoint-io.log`,
`fdatasync.log`, `fsync.strace`, and `strace.log`. Representative initial dump:
`before/transfer-2811230722/stall-00.txt`; final slow runs:
`disk-final/transfer-{2341863676,1975892721,282941204}/`. Initial trace paths in
console output were package-relative; those directories were moved under the
root `.hardening/stall/before/`. The harness now requires absolute trace paths to
avoid that ambiguity. No raw profile or generated binary is committed.

**Remaining blocker:** predictable persistent-storage throughput on this root
volume. No safe application change was demonstrated that removes the observed
sync latency while preserving the existing bounded durable checkpoints. A
repeat on suitable persistent storage (or diagnosis/remediation of this volume)
is still needed before declaring 0071's minimum physical-disk rate satisfied.
Reducing request timeouts cannot fix these captures: the workers were not waiting
on requests. No change to durability policy is proposed or assumed authorized.

Verification before commit: gofmt, `git diff --check`, `go vet ./...`,
`staticcheck ./...`, `go test ./...` and `./scripts/depguard.sh` passed. A separate
64 MiB `-race` benchmark run with tracing enabled also passed, exercising the
watchdog concurrently with stream completion; its instrumentation-heavy rates
are not included in the performance tables. No backlog edits or pushes.

### 0071 conclusion: larger unsynced-budget trial rejected (2026-10-22)

**The 256 MiB mitigation did not remove most disk-tail slowdowns and was
reverted.** Keep the earlier verified-I/O batching/read-ahead and bounded
checkpoint-overlap improvements, not a larger default crash-refetch window.
Targets are met on the measured single-peer and capped-provider **medians** and
on all ten runs of each tmpfs isolation case above. They are **not** consistently met on this shared
physical root volume: destination sync/write latency remains the limiting tail.
Tmpfs establishes the transport/scheduler result, not persistent-storage
performance. This concludes the bounded 0071 tuning experiments without claiming
a universal minimum download rate or concealing the negative disk results.

Trial base: `c822641` after merging master, same Go 1.26.2 / Linux / EPYC machine
and real root ext4/LVM/Kingston NV2 destination as the preceding investigation.
No tmpfs, sysctl changes or storage relocation were used in this trial.
Ten fresh 2 GiB downloads **per scenario** used the same independent provider
caps, verification, final commit, block/mutex profiles and request watchdog.

| Real-disk measurement | Minimum MB/s | Median MB/s | Maximum MB/s | Below target |
|---|---:|---:|---:|---:|
| Previous final set, existing bounded overlap, single | 123.54 | 272.62 | 296.39 | 1/10 |
| **256 MiB candidate, single** | **56.29** | **260.05** | **309.40** | **3/10** |
| Previous final set, existing bounded overlap, capped three/one corrupt | 44.52 | 79.10 | 79.26 | 2/10 |
| **256 MiB candidate, capped three/one corrupt** | **48.52** | **79.01** | **79.29** | **2/10** |

Candidate capped min/median/max fractions of the two honest providers' combined
80 MB/s: **0.607 / 0.988 / 0.991**. All twenty downloads passed size/digest checks;
all ten capped runs rejected the corrupt provider. The single-peer target is
191.86 MB/s from the measured plain-QUIC ceiling; the capped target is 64 MB/s.
The candidate missed **5/20** runs, compared with **9/40** across the preceding
unmodified disk sets. These are unpaired shared-volume observations, not proof
of a precise regression, but they do not support retaining the additional
complexity and larger crash window as an effective mitigation.

The rejected candidate was a genuine decoupled checkpointer, not merely a larger
flush interval:

- A dedicated goroutine continuously drained completed requests into checkpoints,
  starting at the existing 16 MiB threshold (also under budget pressure and at
  finish). All network workers could continue while it synced.
- A configurable per-blob `MaxUnsyncedBytes` defaulted to **256 MiB**. Raw-byte
  credit was reserved **before** a request started, including in-flight and
  duplicate requests, not just chunks that had already finished downloading.
- Only requests completed **before** a checkpoint started could have their credit
  released when that checkpoint succeeded. Requests completing during sync stayed
  charged until a subsequent checkpoint. This conservative accounting bounded
  newly lost raw data by the budget, rather than the budget plus active requests.
- The store's ordering was unchanged: write verified data/proofs, snapshot group
  bits, sync files, then persist that snapshot in SQLite. Thus no group was
  advertised durable merely because a request had finished. Cancellation and
  checkpoint failures did not discard errors; final return still joined the
  checkpointer and committed durably.
- The queue held counters, not payloads. Bytes went through the existing bounded
  protocol buffers into files/page cache. One-second `runtime/metrics` samples
  observed maximum Go heap objects of **28.46 MB single / 29.87 MB capped**, not
  a 256 MiB Go allocation. These are sampled Go-heap values, **not RSS or page
  cache bounds**, and do not capture sub-sample peaks. The heap sample field is
  retained in the optional benchmark watchdog for future measurements.

Deterministic blocked-sync tests checked exact admission limits, snapshot credit
(not releasing newer writes), cancellation, a budget smaller than the checkpoint
threshold, final draining and sync-error propagation. They passed normally and
with `-race`; the existing fetch/proto/store tests passed with the candidate.
No new power-loss/SIGKILL campaign was run for this rejected implementation.

The candidate's slow runs still produced **59 low-throughput dumps**: 58 included
`fsync`, 24 included file `pwrite`, and 24 included budget admission waits (these
categories overlap). Aggregate byte-budget waiting was **77.15 s**. In one capped
run network chunk completion reached the full size around **41.27 s**, but the
fetch returned only at **44.26 s** after durable completion. Increasing the
window neither eliminated device write latency nor removed the final sync tail.
No further budget increases or weakened completion semantics were adopted.

Local evidence is retained under ignored `.hardening/budget256/`:
`download.log`, `download.{block,mutex,test}`, `traces/`, and `candidate.patch`.
The patch contains the rejected implementation, its two deterministic tests and
its temporary `NU_DOWNLOAD_UNSYNCED_BYTES` benchmark override; it applies to the
trial base for reproduction in a disposable checkout. It is **not shipped**.
The measured command, while that patch was applied, was:

```sh
NU_SLOW=1 NU_DOWNLOAD_TRACE="$PWD/.hardening/budget256/traces" \
  go test ./nutest -run '^$' -bench '^BenchmarkDownload$' \
  -benchtime=1x -count=10 -timeout=30m \
  -blockprofile=.hardening/budget256/download.block \
  -mutexprofile=.hardening/budget256/download.mutex \
  -o .hardening/budget256/download.test
```

After the trial, the candidate config, queue, integration, tests and benchmark
override were removed; `fetch/`, `proto/`, `store/` and `nutest/download_test.go`
were verified byte-identical to the merged base. The existing 16 MiB checkpoint
cadence, bounded overlap and durability semantics remain in force. Only this
conclusion and optional test-watchdog heap sampling are retained. No backlog
status was edited; further physical-disk minimum-rate acceptance needs suitable
persistent storage or remediation/characterization of this volume, rather than
an unbounded download buffer.

Final, reverted-tree verification passed: gofmt, `git diff --check`,
`go vet ./... && staticcheck ./... && go test ./... && ./scripts/depguard.sh`.
A 64 MiB tracing-enabled `-race` download run also passed; those instrumented
rates are excluded from the table. No backlog edits or pushes.

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
and verifies the payload (about 1 s locally). That probe uses a static provider result; live Amino
DHT provider discovery against Kubo/the public network is not verified. The CAR import is
exposed through CLI/API and durable operation recovery is tested, but CAR jobs persist the
request/stage/result and replay from the CAR rather than retaining per-block checkpoints.
Cutover readiness and representative-installation reconciliation remain unverified.

## Atrepo member replication (0054) — tree diffs (2026-10-22)

`node.TestAtrepoClusterDataset` runs two real loopback QUIC cluster members. Only
its admin requests PDS exports (exactly two). The storage member acquires both
heads through the production placement fetch path, retains the older version,
fetches the selected small blob, excludes a declared >8 MiB blob, and reports
complete with two-copy readiness after each head. A test-only stream wrapper
counts actual `/nu/tree/1` data frames and payload bytes: first revision **168
frames / 14,909 bytes**, second **7 frames / 834 bytes**, plus 5 have markers.
The second payload is asserted equal to the exact reachable-block set difference,
not merely smaller than the full repository. The account takedown still propagates.

`proto.TestAtrepoTreeDiffAndCursors` checks an update diff (159 frames / 14,470
bytes full, 5 / 1,416 diff), one-frame pages across a layered empty intermediate,
paged have markers, repo-key lookup/range selection and absence proofs.
`fetch.TestAtrepoFallbackPolicyAndRank` checks old-peer layer-1 fallback, both blob
policies, oversized exclusion, unrelated dangling links and ranked tree sources.
Wire request vectors and request/atrepo-stream fuzz seeds run in ordinary tests.
Focused `-race` runs passed. They also exposed a stale reconcile/fetch-completion
window that could retransmit a complete tree; acquisition now rechecks before
starting a transfer rather than trusting that stale completeness result.

Parallel full-suite attempts hit unrelated timeouts in `cluster.TestDrainRemove`
(local add at 180,224/204,800 bytes), `cluster.TestPrivateCluster` (join) and
`nutest.TestPublishSeedWait` (local add). Serial reruns of the complete cluster and
nutest packages passed without changing their code/timeouts. Final gates passed:
gofmt, `git diff --check`, `go vet ./... && staticcheck ./... && go test ./... &&
./scripts/depguard.sh` (the final full-suite invocation used Go's successful test
cache). No live network, prolonged fuzzing or benchmark run was needed.

Legacy blob refs without a declared size remain honest incomplete obligations
when unavailable: members cannot infer a >8 MiB exception from missing bytes.
This evidence does not cover the separate multi-day live-account acceptance,
public `/at/` serving or per-account resource budgets.
