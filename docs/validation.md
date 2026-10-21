# Validation and measurements

[Spec overview](../spec.md) · [Product brief](product.md) · [Operations](operations.md) · [Networking](networking.md)

**No field connectivity or scale results are recorded yet.** Specification figures are
hypotheses and sizing targets, not measured capabilities. Record reproducible results here
(or link reports indexed here), including negative results. Synthetic regression tests do
not establish field success.

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

`miniupnpd` and a standalone PCP server were unavailable locally; passwordless sudo was
unavailable. **Real-daemon interop remains deferred to the on-demand natlab CI workflow**,
which now installs miniupnpd and runs the portmap/netmon lab binaries as well as the base
lab. Current automated mapping scenarios still select deterministic in-test servers;
installation alone does not claim daemon interop. Fake lease expiry is not a kernel lease
scheduler: renewal timing is tested separately at the client boundary. IPv6 firewall/
PEER forwarding, UPnP IPv6FirewallControl, real suspend/resume and consumer-router field
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
