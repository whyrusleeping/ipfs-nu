# Pilot workloads, benchmarks and decision gates (0034)

[Spec overview](../spec.md) · [Product brief](product.md#audience-and-jobs) · [Validation](validation.md#pilot-workloads) · [Operations](operations.md#acceptance-scenarios)

**Status: templates awaiting real pilots.** Everything here was prepared by coding agents
as method, fixtures and templates. No pilot team has been recruited through this
document, no real workload is recorded in it, and none of its thresholds is decided.
The one recruited pilot so far, CollGS, is recorded separately in
[validation](validation.md#pilot-candidates). Synthetic results produced by the harness
are labeled **harness smoke results, not pilot evidence**. They exercise the method and
can expose mechanisms such as whole-file amplification. They do not support claims about
capacity, performance or scale. Supported limits come only from measured pilot workloads
([milestones](../spec.md#milestones)).

Agents own the fixtures, the runner, the methodology and the templates. People own
recruiting pilots, recording their workloads, running on their hardware and taking the
gate decisions below.

## Workload matrix

Each row says what to measure and which journey or shape it stands for. The synthetic
fixture (`bench/fixtures`) is the reproducible stand-in. A pilot's own data replaces it
whenever the pilot can share data or measurements.

### First-magic journeys (individual and publisher)

| # | Journey | Fixture | What it measures | Gate |
|---|---|---|---|---|
| J1 | Install, add a file or tree, share the link; the recipient runs `get` with no daemon and no init | `site` v0 | Setup steps and time, add time, time to the complete verified tree, served bytes, failures and their next-step messages | v0 (LAN/routed), 1 (declared networks, 10 GB/10k files) |
| J2 | Browse the shared tree in the local gateway (`<cid>.localhost`) | `site` v0 | Time to the first page, whether relative links and assets work, and whether the origin is isolated from the API | v0, 1 |
| J3 | Publisher updates: the same recipient fetches later versions | `site` v1… | Bytes served per update compared with novel bytes, and update time | 1 |
| J4 | Keep it available: register a seed, wait for the exact target, sleep; a fresh machine opens it | `site` | Time to `held` on the seed, and success while the publisher sleeps | 2 (names after atproto login) |
| J5 | Mirror a changing dataset (`follow`) | `dump`, `items` | Served-snapshot consistency, visible lag, bounded resources, resumption | 2 |

### Institutional archive and mirror shapes

| Shape | Stands for | Per version (fixture default) | Main metric | Whole-file expectation |
|---|---|---|---|---|
| `sparse` | Independently changing small objects: documents, records, thumbnails | 1% of 2,000 files (256 B–256 KiB) rewritten, 0.5% added, 0.2% removed | Per-file overhead (structure, DB, requests, readiness evaluation) and directory-diff cost | Amplification about 1× (files are small and wholly new) |
| `rewrite` | Monolithic in-place rewrites: SQLite, HDF5, disk images | 16 × 4 KiB pages plus 64 KiB of growth in a 32 MiB file | Stored and transferred bytes per novel byte | Amplification ≈ file size ÷ change (about 250× by default) |
| `shift` | Shifting inserts: an uncompressed tar or text export | 3 records of 2–16 KiB inserted in 16 MiB | Same as `rewrite`, but only content-defined chunking could recover these bytes | Amplification ≈ file size ÷ insert |
| `append` | Logs, WAL segments, JSONL feeds with rotation | Each of 4 logs grows 256 KiB; logs over 4 MiB rotate by rename | Append amplification, which grows with the file; how rotation bounds it | Amplification ≈ active log size ÷ append; closed segments cost nothing after rotation |
| `sharded` | Hash-partitioned key/value or search-index shards | 464 record changes of 64 B spread over 256 × 64 KiB shards | How small logical changes fan out into whole-shard changes | Amplification ≈ shards touched × shard size ÷ change |
| `dump` | Periodic Wikipedia-dump-like snapshots: dated directories, a `latest/` copy, 3 dumps kept | A new dump of 10 parts; a quarter are byte-identical to the previous dump | Storage growth per snapshot, deduplication across directories, retention reclaim | About 1× (compressed parts change wholesale; identical parts deduplicate) |
| `items` | Internet-Archive-collection-like `items/<id>/` trees with metadata, originals and derivatives | 10% new items, metadata edits on 3%, derivatives on 1%, 0.5% removed | Growth and per-item overhead; small metadata edits next to large unchanged originals | About 1× |
| CollGS | Earth-observation scene collection (D22) | See [validation](validation.md#collgs-pilot-preview-0063) | Ingest items/day, catalog and index cost | — (a recruited pilot with its own criteria) |

Across all shapes, the runner measures:

- add/update cost: wall time and process CPU;
- transfer bytes per version;
- storage growth per version, deduplication and DB growth;
- behaviour under a too-small budget;
- retention reclaim;
- exact-version restore time and verification;
- repair after a member loss: detection time, restore time and bytes moved;
- the operator interventions each step needed.

### Pilot success criteria per gate

These are templates. Each threshold marked *(pilot)* is agreed with the pilot team before
its run and recorded in its intake. Agents do not choose these values.

| Gate | Criteria to record for each pilot |
|---|---|
| v0 | J1–J2 complete on the pilot's LAN or routed network without help. Record steps, time and every error message, and whether it named a next step |
| 1 — First magic | J1–J3 for fresh users on declared networks: completion rate, help requests and time to the first verified file *(pilot)*. 10 GB/10k-file tree between the pilot's two laptops. No account or network configuration |
| 2a — CollGS | The criteria in [validation](validation.md#pilot-candidates) |
| 2 — Real users | The team's own data on a one-node archive: every version restored and verified exactly. The update cost of each shape the pilot reports is within the pilot's stated transfer and storage budget *(pilot)*. On 3+ members: zero false `ready`, repair after a member loss with no data-plane intervention, and every remaining intervention prompted by a status or alert message with an action. Update latency is within the team's update window *(pilot)* |
| 3 — v1 | Support limits published from the pilots' measurements (entries, bytes, versions, members). Pilots with IPFS dependencies complete a verified import before cutover ([migration](migration.md)). Upgrade and restore are exercised on pilot hardware |
| 4 — Scale | Representative update, storage and repair costs measured at progressively larger pilot sizes; no extrapolation from smoke results |

## Pilot intake template

Copy this for each candidate (`docs/pilots/<team>.md` or a linked report). Fill in only
what the team states or measures, and mark unknown fields *unknown*. Record consent to
publish each figure.

```text
Pilot: <team / contact / date / consent to publish: yes|aggregated|no>
Audience & job: individual | publisher | mirror operator | archive team | collaborating operators | administrator
Current tools: <rsync, S3, BitTorrent, Kubo/IPFS Cluster, tape, …>  Why change: <…>

DATA
  Total bytes / entries now; growth per month:
  File-size distribution: p10 / p50 / p90 / p99 / max; files > 8 MiB (%):
  Formats: compressed? databases/containers (SQLite, HDF5, Zarr, tar)? logs/JSONL? images/video?
  Directory shape: depth, largest directory fan-out, items-per-collection:
  Duplication: across files / across versions (estimate or measured):
CHANGE PATTERN (per update)
  Frequency (hourly/daily/periodic dumps):
  Files added / modified / removed; bytes in them; bytes actually different:
  Kind: new files | in-place page writes | appends | full rewrites of compressed files | shard updates | renames
  Closest fixture shape(s) and fixturegen parameters approximating it:
RETENTION & RESTORE
  Versions to keep (schedule, legal holds, labels); historical reads expected:
  Restore expectations: which versions, how fast (team-stated), how often tested:
TOPOLOGY
  Members/sites/zones; failure domains; who may hold what:
  Links: bandwidth, RTT, loss; NAT/firewall/VPN; multicast available?
HARDWARE
  Per member: CPU, RAM, disk type/capacity/free, filesystem, OS/version:
OPERATIONS
  Staff/on-call; change windows; monitoring in use; backup/export policy:
  Security/compliance constraints; members trusted?
IPFS DEPENDENCIES
  Kubo/IPFS Cluster versions; pinset size; CIDv0/v1; UnixFS chunker/raw-leaves; IPNS/DNSLink;
  gateways and published CIDs that must keep resolving; Bitswap peers outside the team:
SUCCESS
  Team's own definition of success; budgets (transfer/storage per update); deadlines:
```

## Benchmark fixtures and runner

- `bench/fixtures` generates the shapes deterministically from a seed, as listings of
  pseudo-random incompressible content. `Params.Scale` multiplies each shape's dominant
  dimension (`fixturegen -list` names it). The defaults are a few MiB to about 50 MiB per
  version. `Change` reports, per version, the whole-file bytes (added plus modified
  files) and the **novel bytes**: content that appears nowhere in the previous version.
  Novel bytes are the floor that any deduplicating store must take in.
- `bench/cmd/fixturegen` writes a version to a directory for runs on real machines:
  `fixturegen -shape rewrite -version 0 -out src`, then
  `fixturegen -shape rewrite -from 0 -version 1 -out src`, and so on. It prints the
  version's `Change` as JSON.
- `bench/pilot` (`TestPilot`) is the opt-in runner. It is skipped unless
  `NU_BENCH_PILOT` is set:

  ```sh
  NU_BENCH_PILOT=all NU_BENCH_OUT=/tmp/pilot-out go test ./bench/pilot -run TestPilot -v -timeout 1h
  # one shape, larger, on disk instead of tmpfs:
  TMPDIR=/var/tmp NU_BENCH_PILOT=rewrite NU_BENCH_SCALE=8 NU_BENCH_VERSIONS=6 go test ./bench/pilot -run TestPilot -v
  ```

  For each shape it runs:
  1. **One-node archive:** an `--archive` dataset with `keep_last=2` (each version is
     imported from a source directory, as `nu dataset update <ds>` does). It records the
     change, add-and-update time and CPU, and the store deltas (blobs and outboards, DB,
     physical bytes). It then restores the oldest and newest versions exactly and
     verifies them byte for byte. Next it runs an update with `store.limit` set to leave
     half of that update's whole-file bytes: the update must be refused with
     `insufficient_storage`, every held version must stay complete and readable, and the
     update must succeed after one intervention (raising the limit). Last, it prunes and
     runs GC, recording the dry-run estimate against the physical bytes reclaimed.
  2. **First magic** (non-institutional shapes): a sharer daemon, a recipient with no
     daemon and no init, and a browser daemon serving the gateway page. Each later
     version is fetched by the same recipient, and the runner records the bytes served.
  3. **Three-member cluster** (nutest in-process, loopback QUIC, private mode): an
     archive dataset with replicas 2, written by the first member. For each version it
     records the time to `ready` and to `held` by every owner, the data-plane bytes
     served by all members, and the growth of total physical bytes. Afterwards the
     writer releases its surplus (`dataset gc`, an intervention), a member holding only
     its own share restores the oldest version, and one member is killed. The runner
     then times detection (`unavailable`) and the restoration of two confirmed copies
     by the survivors, with the founder as the online repair authority, and records the
     bytes this moves.
- **Transfer floor.** With the writer as one of n members and k copies per blob, the
  expected transfer is k(n−1)/n × novel bytes: 1.33× for k=2, n=3. Higher values come
  from whole-file amplification and protocol overhead. Served bytes are counted on the
  `/nu/bytes/1`, `/nu/have/1` and `/nu/tree/1` response streams (`nutest.Node.Served`),
  excluding cluster-state sync and QUIC/TLS overhead.
- **Compressed time.** The cluster heartbeat is 200 ms, evidence stays fresh for 2 s
  and `down_after` is 1 s. Readiness and repair durations therefore scale with these
  windows. In production, detection waits `down_after` (15 min by default). Per-file
  costs (add, readiness evaluation, restore) are not compressed.
- **Not covered yet.** The runner does not exercise names, publisher seed waits (J4),
  follows (J5), the netns NAT lab, physical machines, crash or power loss during an
  update (covered by unit and scenario tests, not measured here), or upgrades.
  Multi-machine runs use `fixturegen` with the CLI:

  ```sh
  fixturegen -shape items -version 0 -out ~/fx/src
  nu dataset create fx --archive --source ~/fx/src     # first version
  for v in 1 2 3; do fixturegen -shape items -from $((v-1)) -version $v -out ~/fx/src; nu dataset update fx; done
  nu dataset versions fx; nu get fx@<tid> -o /tmp/restore   # time it; compare with fixturegen output
  # clusters: nu cluster dataset create fx --archive --replicas 2; nu cluster dataset wait fx <cid> --condition ready
  ```

## Result templates

The runner writes `results.json` (every measurement) and `results.md` (the summary tables
in [validation](validation.md#harness-smoke-results-0034-not-pilot-evidence)) to
`NU_BENCH_OUT`. A pilot report uses the same columns and adds the
[reporting method](validation.md#reporting-method) fields:

```text
Run: <pilot / date / commit / operator>   Evidence class: pilot | harness smoke (not pilot evidence)
Environment: OS/arch, CPU, RAM, disk + filesystem, network path (bandwidth/RTT/loss), members/zones
Workload: pilot data | fixture <shape> seed <s> scale <x>; versions; total/novel/whole-file bytes per version
Per version: add+update s, CPU s, Δphysical, ΔDB, novel, whole-file, amplification, transfer, ready s, held s
Restore: version, bytes, seconds, verified (y/n), bytes fetched from peers
Budget: limit, need, refused (code/action text), held intact, interventions to resume
Retention: pruned, estimate, reclaimed
Repair: member lost, detect s, restore s, bytes moved, final copies, alerts seen
Interventions: every manual step (command + reason + whether a status/alert message prompted it)
First magic: steps, time, failures + their messages, help requested, user comments
Repetitions / variance / failures / uncertainty:
```

## Decision gates

These are decision criteria to evaluate against real pilot data. They decide nothing.
The decision belongs to the project owners, made with the pilots' measurements in hand.
The proposed triggers are starting points for that discussion, not adopted thresholds.

### Sub-file reuse (chunking within files)

Today a file is one blob. A change of any size stores and transfers the whole file again
([operations](operations.md#t-transfer-budgets)), even though Bao chunk groups already
verify sub-ranges within a blob. The harness shows the mechanism on synthetic shapes:
monolithic rewrites, shifting inserts, appends and sharded indexes cost whole files per
version (smoke results in [validation](validation.md#harness-smoke-results-0034-not-pilot-evidence)).
Whether this matters depends entirely on real workloads.

Evaluate per pilot:

- **Amplification:** Δphysical ÷ novel bytes and transfer ÷ (transfer floor × novel),
  per update and over a retention window. Measure them on the pilot's data, or on a
  fixture parameterized from its intake.
- **Share:** the fraction of the pilot's update bytes that falls in amplifying shapes
  (`rewrite`, `shift`, `append`, `sharded`). Exclude compressed outputs that change
  wholesale (`dump`): only a new format could help those.
- **Budget impact:** whether the amplified cost exceeds the team's stated per-update
  transfer or storage budget, or its update window.
- **Workaround cost:** whether the team can restructure its data cheaply instead, for
  example by rotating logs into segments or splitting shards smaller.

Proposed trigger for moving sub-file reuse forward (to be confirmed or replaced):

1. At least one gate-2 pilot shows amplification of 10× or more on data that makes up
   at least 20% of its update bytes;
2. the amplified cost exceeds that pilot's stated budget or window; and
3. no cheap data-layout workaround is acceptable to that pilot.

If it triggers, the evidence also chooses the kind of reuse. Fixed-size reuse aligned to
chunk groups covers `rewrite`, `append` and `sharded`. Only content-defined chunking
covers `shift`. Structural delta sync stays distinct from arbitrary binary deltas.

### Capacity-adjustment UX

Today a `store.limit` shortfall pauses or refuses new content with `insufficient_storage`
and an action. It keeps every held version, and the operator raises the limit or prunes
(C1–C5). The harness checks this behaviour, not its usability. Evaluate with pilots:

- how often each pilot hits pressure or a quota, and why: growth, retention, staging or
  surplus;
- the time and number of steps to resolve it, whether the message's action sufficed,
  and whether people needed help;
- how far the reclaim estimate (prune or gc dry-run) differs from the space actually
  freed. In the smoke run, small-object shapes reclaimed less than estimated because the
  SQLite file does not shrink;
- whether teams need preflight (`dataset update --dry-run` with required headroom),
  per-dataset quotas, automatic pruning to a byte budget (`max_bytes`), guided limit
  changes in status, or DB compaction.

Proposed trigger for capacity-UX work ahead of other gate-2 items (to be confirmed):
any pilot operator needs outside help to resolve a capacity event, or a resolution takes
more than one documented command, or reclaimed space differs from the estimate by more
than 25% on the pilot's data.

### Per-entry overhead (from the harness; evaluate with pilots)

The smoke run showed per-file costs that dominate small-object shapes. Updating a
2,000-file tree took several seconds, mostly per-blob store lookups in readiness
evaluation and archive establishment (`store.Has` through `replica.Manager.check`).
Restore and cluster readiness behave similarly. Before support limits on entry count are
published, measure update latency against the pilots' entry counts, versions kept and
update windows.

## What remains for people

- Recruit individual/publisher and institutional pilots: archive or mirror teams with
  smaller Internet-Archive/Wikipedia-shaped workflows. Fill in the intake for each and
  agree the *(pilot)* thresholds.
- Run the journeys with real users (J1–J5), including first-run observation, and record
  the results with the template.
- Run the runner, or `fixturegen` with the CLI, on the pilots' hardware and networks,
  and on the pilots' own data where shareable.
- Decide the sub-file reuse and capacity-UX gates, and later the support limits, from
  that evidence.
