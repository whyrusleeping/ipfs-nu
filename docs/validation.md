# Validation and measurements

[Spec overview](../spec.md) · [Product brief](product.md) · [Operations](operations.md) · [Networking](networking.md)

**No results are recorded yet.** Specification figures are hypotheses and sizing targets,
not measured capabilities. Record reproducible results here (or link reports indexed here),
including negative results. Synthetic regression tests do not establish field success.

## Early experiments

| Experiment | Decision/evidence required |
|---|---|
| Amino BLAKE3 provider and swarm keys (Q2) | Acceptance/retrieval against deployed servers, protocol citizenship with non-Bitswap providers, explicit routing fallback |
| Stock-quic-go Nu-path | Relay ↔ direct ↔ reattached relay during transfers, RTT/loss/MTU changes and network loss; no fork assumed |
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

## Release evidence

Index results for [milestone](../spec.md#milestones) exits, the [operational acceptance
scenarios](operations.md#acceptance-scenarios), interoperability, fault/security review,
macOS/Linux portability, migration and recovery. User testing must include fresh users who
have not read the protocol docs. Record confusing errors and manual workarounds as failures
of the intended easy path, even when an expert can eventually complete the transfer.
