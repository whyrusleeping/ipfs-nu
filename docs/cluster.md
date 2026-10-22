# Clusters

[Spec overview](../spec.md) · [Data and replication](data.md) · [Names](names.md) · [Networking](networking.md) · [Following](follow.md)

A **cluster** is a group of Nu nodes under shared administration that together store a set
of datasets according to a replication policy and track the datasets' updates over time. It
succeeds IPFS Cluster: a shared pinset with placement, status and repair. Unlike IPFS
Cluster, it is part of the daemon rather than a sidecar, places data per blob instead of
pinning whole DAGs on every allocated peer, and can run in a private or fully isolated network
without a DHT, public relays or atproto services.

A node following a dataset ([D19](follow.md)) is the one-node case. A cluster adds signed
shared state, membership, placement across members, handoff and repair. Members reuse D19's
delta sync, completeness marks and atomic switches, restricted to the blobs they own.

**Local cluster.** Every daemon has an implicit one-member **local cluster**, created on first
use with its own root/admin key in `$NU_PATH`. It has no network exposure, so it coexists with
any mode. `nu dataset …` operates on it with the same verbs, records, presets, history and
status as `nu cluster dataset …`; follows are mirror-preset datasets in it with a `name`
source. A one-node archive is therefore `nu dataset create photos --archive`, with no cluster
administration. A daemon additionally belongs to at most one networked cluster in v1.
Dataset names match `[a-z0-9-]+` and are unique per daemon across its local and networked
cluster, so `<dataset>@<version>` and `/c/<dataset>` are unambiguous. Moving a local dataset
into a networked cluster is an explicit export/import of its versions, not a merge of authorities.

## D20. Clusters

### Design target

Basic versioned archival is a first real-user workflow, not gated on public names or hard
NAT. An individual can use the same archive preset with one storage member, with honest
single-copy status; adding members changes placement, not the content model. The [product
brief](product.md) and [operational contracts](operations.md) define the experience.

These figures are scale-track sizing targets, not initial supported limits or measured
results. Pilot limits come from [validation](validation.md):

| Quantity | Target |
|---|---|
| Members | 3–100 storage members per cluster, plus clients |
| Datasets | 10k per cluster; single datasets up to the [D19 target](follow.md#design-target) (10M entries) |
| Stored | about 1 PB in aggregate; about 100M blobs per member |
| Cluster state | 10k datasets and 100 members in a few MB; a new member syncs it in seconds |
| Repair | starts within `repair.down_after`; lost copies are recreated in parallel from all survivors |

### Concepts

| Term | Meaning |
|---|---|
| Cluster | Identified by the CID of its genesis record; owns keys, members, datasets and policy |
| Key | A P-256 `did:key` (atproto-valid; hardware/Secure Enclave-backed where available) held by a person or automation: `root`, `admin`, or `writer` for listed datasets |
| Member | A node (PeerId) admitted by an admin, with role `storage` or `client` |
| Dataset | A named pinset entry: a source that yields a current target, plus a placement policy |
| Head | The current target of a cluster-local dataset |
| Version | A past or current head recorded in the dataset's version tree when history is enabled |
| Placement | A deterministic function of cluster state that maps each blob to its owning members |

Authority belongs to keys, not nodes. A node can hold keys; for example, an admin laptop is a
`client` member holding an admin key. Nodes authenticate as members with their PeerIds.
The exception is [collections](collections.md): beyond their guaranteed placement, each
member's operator chooses which items the member also holds.

### Cluster state

Each authorized key has its own **state repository**: a byte-exact atproto v3 repository
(SHA-256 MST, signed commits with `rev`; [D21](dasl.md#atproto-repository-profile)) signed
by that key's `did:key` and served only inside the cluster. It is not hosted on a PDS or
visible on the firehose, which lets the same design run air-gapped. Its CAR export can be
inspected and verified with stock atproto tooling.

```text
io.ipfs.nu.cluster.key/<did>          { role: "admin"|"writer", revoked?: {after: <commit cid>}, hlc }
io.ipfs.nu.cluster.member/<peerId>    { roles, capacity, store_limit, tags?: {zone: …}, addrs?, label?,
                                        state: "active"|"draining"|"unavailable"|"removed", hlc }
io.ipfs.nu.cluster.invite/<id>        { proofKey, roles, tags?, capacity?, expires, hlc }
io.ipfs.nu.cluster.dataset/<name>     { source, policy, writers?: [<did>], collection?, note?, hlc }
io.ipfs.nu.cluster.head/<name>        { target: "<cid>", kind, size?, note?, hlc }
io.ipfs.nu.cluster.drop/<name>        { paths: [<item path>], hlc }          # union collections, D22
io.ipfs.nu.cluster.history/<name>     { tree: <version-tree cid>, hlc }      # see Version history
io.ipfs.nu.cluster.config/self        { defaults, repair, clock, hlc }
any key → { deleted: true, hlc }       # tombstone
```

- **Genesis:** `nu cluster init <name> --mode public|private|isolated` creates a root key and
  the genesis record `{name, mode, rootKey, createdAt}`. The cluster ID is the genesis CID.
  The root key is exported for offline storage. By default the init node also receives an
  admin key and becomes the first storage member.
- **Merge:** the cluster state is a last-writer-wins map over all authorized repositories.
  For each record key, the entry with the greatest `(hlc, signer did)` wins; tombstones
  compete the same way and are retained. `hlc` is a hybrid logical clock value (physical
  milliseconds plus a counter), so a writer with a lagging clock still orders after the
  state it has seen. Records more than `clock.max_future` (default 5 min) ahead are held back.
- **Authorization:** root and admins may write any record. Writers may write only
  `head/<name>` and `history/<name>` for datasets that list them. The root key is fixed by
  genesis and cannot be revoked or replaced by a record. Out-of-scope records are ignored.
  A revocation names the revoked key's last accepted commit, and later commits from that key
  are ignored. Root-signed `key` records override
  admin-signed ones, which resolves disputes such as two admins revoking each other.
- **Replication:** every member stores and serves every state repository. When connected
  members meet, they exchange `{did → (rev, commit cid)}` heads, then fetch newer commits as
  atproto repo diffs (`getRepo?since` CAR slices) against the commits they hold, over
  [`/nu/cluster/1`](#nucluster1). Changes are also pushed to watchers. A
  write from a client returns after one storage member acknowledges **signed state**; a
  metadata wait can require more. This does not confirm stored content. Exact-version
  content/policy waits are separate and return evidence, age, epoch and unmet requirements.

Because each repository holds its signer's current state rather than an operation log, a new
member syncs O(state), not O(history). This avoids the uncompactable Merkle-CRDT DAG of
IPFS Cluster's CRDT mode and needs no Raft majority. Concurrent writes to the same record
resolve by LWW; there are no multi-record transactions. CLI/API outcomes must expose the
resulting head and displaced intent. An observed-head guard is not global compare-and-swap;
conflict detection/reconciliation remains protocol design work. Archive history preserves
concurrent snapshots subject to explicit retention and content availability.

#### State encoding and evaluation

- **HLC:** a TID string (atproto TID syntax, clock-ID bits zero) whose timestamp field counts
  microseconds since the Unix epoch; values compare as integers. A writer stamps
  `max(now_µs, last + 1, seen + 1)`, where `seen` is the greatest `hlc` in its merged state,
  so the logical counter lives in the sub-millisecond digits.
- **Records** are DRISL maps whose `$type` is their collection, each with an `hlc` (except
  genesis). Unknown fields are ignored; a record that fails its schema or record-key rule is
  ignored like an out-of-scope one. A **tombstone** is `{$type, deleted: true, hlc}`.

| Collection (record key) | Fields |
|---|---|
| `genesis` (`self`, root repository) | `name`, `mode` (`public`, `private`, `isolated`, or `local` for the implicit local cluster), `rootKey` (did:key), `createdAt` (datetime). No `hlc`; immutable; the cluster ID is its CID |
| `key` (`<did:key>`) | `role` (`admin`, `writer`), `revoked?` `{after: <commit link>, rev: <TID>}` |
| `member` (`<PeerId base58>`) | `roles` ([`storage`\|`client`]), `capacity` (int), `store_limit?`, `tags?` (string map), `addrs?` (≤ 16 multiaddrs), `label?`, `state` |
| `invite` (`<id>`, 16 base32 chars) | `proofKey` (32 bytes, [below](#invites-and-join)), `roles`, `tags?`, `capacity?`, `expires` (datetime) |
| `dataset` (`<name>`, `[a-z0-9-]{1,64}`) | `source` (`{local: true}`), `policy` (`{replicas, all?: true, where?: {<tag>: [<value>…]}, spread?: <tag>}`, at most 16 `where` tags of 1–16 values; other policy fields above are later work), `writers?` ([did]), `collection?` (`{facets?: {<name>: {ptr, type}}, merge?: "union"\|"lww", expire?: {facet, after}}`, [D22](collections.md#items-and-facets)), `note?` |
| `head` (`<name>`) | `target` (CID string), `kind` (`tree`, `file`, `blob`), `size?`, `note?` |
| `drop` (`<name>`, admins) | `paths` (at most 256 item paths; the union head leaves out everything at or below them, [D22](collections.md#union-heads)) |
| `config` (`self`) | `clock?` (`{max_future_ms?, warn_skew_ms?}`) |

- **Authorization:** root writes any record but `genesis` after the first; admins write `key`
  (never about the root key), `member`, `invite`, `dataset`, `head`, `history`, `drop` and
  `config`; writers write `head/<n>` and `history/<n>` only while `dataset/<n>` lists them.
  Any other record is out of scope and ignored.
- **Evaluation** is a pure function of the held repositories and the clock:
  1. The root repository is the one whose DID is `genesis.rootKey`.
  2. Keys: starting from `{root}`, merge the `key` records of the root and current admin
     repositories (a root-signed record beats any admin-signed one for the same key; ties
     otherwise by `(hlc, signer)`), and take every non-deleted `admin`/`writer` key as
     authorized. Repeat until the set is stable; after 16 rounds without a fixpoint only
     root-signed keys count.
  3. A key with `revoked` is evaluated at commit `after` only: commits with a greater `rev` are
     refused, a member holding a later commit fetches `after`, and until `after` is held the
     repository contributes nothing. Its earlier records still count.
  4. All other records merge by LWW over the authorized repositories under the matrix.
     Records with `hlc` more than `clock.max_future` ahead are held back until then.
- **Writer heads:** `head/<n>` of every authorized signer stays readable per signer (union
  heads, [D22](collections.md#union-heads)); the LWW winner is the head of a `merge: lww`
  dataset.
- **Storage:** repository blocks live in the store, each held head pinned (`cluster`
  source, `atrepo` profile); the held heads per cluster and the node's authority keys
  (0600) live under `$NU_PATH/cluster/`. The local cluster's root key writes its records.

### Datasets and updates

| Source | Target |
|---|---|
| `{local: true}` | The merged `head/<name>` record, written by admins or listed writers |
| `{name: "at://…/io.ipfs.nu.name/<n>"}` | An external Nu name, followed as in D19; requires name-plane access or [bundles](#air-gap-bundles) |
| `{atrepo: "<did>", nsids?, blobs?}` | The latest verified commit of an atproto account ([D21](dasl.md#atproto-accounts-as-archive-datasets)); third-party history is private by default |
| `{local: true, ingest?: {hosts, limits}}` | A collection's union head, fed by producers pushing items to writer members ([D22](collections.md#ingest)); `hosts` are at most 256 exact host names or IP literals, `limits` is `{max_bytes?, attempts? (≤ 100), timeout?}` per URL file (defaults 1 TiB, 6, `2h`) |

A fixed CID pin is a local dataset whose head never changes: `nu cluster pin <cid>`.

**Presets (0047).** `nu [cluster] dataset create <ds> --archive|--mirror [--source <dir>]`
writes ordinary policy fields, nothing preset-specific: `--archive` sets `history` to the
default schedule below (visible in the record and in `dataset status`); `--mirror` leaves
`history` unset (current state with the protected-version safeguard). Both keep the scope's
`replicas` default: `k=1` on the one-node local cluster, shown as one copy (never "two
copies"), and the team default otherwise. `--source <dir>` stores `source.path`, a
writer-local absolute directory: `nu dataset update <ds>` without a target re-adds it
(incrementally, through the import cache) and writes the new head. Name and atproto sources
(0045, 0054) are further `source` variants; their versions record `observed` provenance.

| Policy | Meaning |
|---|---|
| `replicas` | Copies per blob: team default `k=2` or explicit `all`; one-node archive preset visibly requests `k=1`; collections may set `0` (no guaranteed copies) |
| `where` | Tag filter selecting eligible storage members, e.g. `zone in [eu-1, eu-2]` |
| `spread` | Required distinct tag values. Team default `zone` is fixed at creation only if all eligible members are tagged and enough distinct zones exist; otherwise choose/warn. One-node preset explicitly has no spread requirement |
| `path` | Replicate only a subtree, as in D19 scopes |
| `max_size` | Logical referenced-byte quota per dataset/scope; separate physical and transfer budgets apply |
| `history` | Explicit [version retention](#version-history): enabled by archive preset, off for mirror preset |
| `announce` | Public mode only: provide the swarm key and roots publicly (default off) |
| `publish` | Public mode only: mirror the head to an atproto name once it is replicated |
| `collection` | Items with metadata facets, member-selected holdings additive to placement, `merge: union` heads, `expire` ([D22](collections.md)) |

Writers change a local dataset by writing a new head:

```text
nu cluster dataset add <ds> -r ./dir      # incremental import (D19), then write head
nu cluster dataset edit <ds> --put p=<cid> --rm p
nu cluster dataset update <ds> <cid>
```

Members learn about the new head through state replication and delta-sync toward it.
A collection with several writers merges their heads instead of choosing one by LWW
([union heads](collections.md#union-heads)).

- **Protected-version rule:** planned updates keep the last policy-ready head pinned until
  fresh authenticated evidence confirms the new target at the required copies/domains in
  the applicable policy/placement epoch. Only then may old unique, non-retained blobs be
  released. A failure may reduce available copies immediately; repair depends on surviving
  content, connectivity, eligible capacity and online repair authority. Unknown/stale or
  conflicting evidence cannot authorize release. Intermediate heads may be skipped, but
  archive-selected versions remain retention obligations with visible incomplete status.
  The partition/concurrent-epoch evidence protocol must be specified and fault-tested
  before safe release is claimed; LWW state convergence alone is not that protocol.
  If no version was ever policy-ready, protect the last fully held usable view and pause
  policy advancement; keep at most bounded incoming staging, not every observed head.
  Explicit retained snapshots remain obligations. A one-node preset can be ready at its
  explicit one-copy policy, but never implies redundant storage. The evidence and release
  rules are [E1–E10 and S1–S8](operations.md#e-readiness-evidence); `release.auto` is off
  until 0048's tests pass, so releases are dry-run reports only.
- Without `history`, a dataset tracks current state as names do. Public `nu.name` records
  remain current-state either way; cluster history is served to members, and outside the
  cluster only through `announce` or bundles.
- Removing a dataset writes a tombstone. Members release its unique blobs after
  `dataset.remove_grace` (default 24 h), allowing undo while content still exists. Destructive
  dry runs show affected versions/content and remaining references; metadata restoration
  after GC does not recover deleted bytes.

### Version history

The archive preset enables `policy.history` with an explicit visible retention schedule;
the mirror preset follows current state with the protected-version safeguard. Exact preset
schedules require pilot validation, not silent unbounded retention. Retained versions share
structure/blobs through content addressing. O(changes) adding/pruning/restoring is a design
target to benchmark, not yet an established bound.

Each version separately reports observed, fetching, retained-complete, degraded/unknown or
skipped/never-completed or metadata-only/pruned content. A history entry is not proof the
bytes were ever replicated.
Time/label lookup selects a version and reports its availability, rather than silently falling
back to another snapshot. Version HLC times are ordering metadata, not trusted timestamps.

**Version tree.** Each dataset with history has a version tree: the version records
`io.ipfs.nu.cluster.version/<name>.<tid>` of the authorized signers' state repositories,
keyed by version TID (the author's HLC when it wrote the version) and therefore stored in
the state MST itself (0047; a separate MST behind `history/<name>` would need its own sync
path, so `history/<name>` stays reserved):

```text
version/<name>.<tid> → { root: "<cid>", kind, parent?: "<tid>", author: "<did>", files,
          bytes, newBytes, restoredFrom?: "<tid>", observed?: {rev, commit},
          imported?: {source, manifest}, label?, note?, hlc }
```

- A writer that writes a new head also writes the version record in the same commit, so
  the signed state commit authenticates every entry without separate signatures; the
  version records are kept apart from the `head` record, so a label edit cannot overwrite a
  newer head. `root` is a CID string (like `head.target`), so the version tree is metadata
  only: content is pinned per retained version, never through the tree. `files` and
  `bytes` come from the tree header (blob/file node: 1 and its size); `newBytes` is the
  size of blobs absent from the parent version, computed by the writer from both walks.
- `imported` (0042) is set by an import with a dataset: `source` is `ipfs://<canonical
  original root CID>` and `manifest` the CID of the DRISL provenance manifest
  ([migration.md](migration.md#resumption-provenance-and-cutover)); it is part of the
  identity, so the state commit's signature authenticates the reference.
- **Identity, annotations and authority (0047).** A version's identity is its TID plus
  every field except the annotations `label`, `note` and `hlc`, compared as the DRISL
  encoding of the record without those keys (unknown fields included). Keys that may write
  the dataset's head (admins and listed writers) may write and annotate its versions.
  Annotating another author's version copies its identity fields into the annotator's own
  record. Merge per TID: the *canonical* record is the earliest (`hlc`, signer) record
  signed by its own `author`, else the earliest record; records whose identity differs are
  **conflicts**, shown with the version and never merged into it; among the agreeing
  records and the tombstones the greatest (`hlc`, signer) supplies the annotations, and a
  winning tombstone compacts the version's metadata. Labels match `[a-z][a-z0-9._-]{0,63}`
  and must not parse as a TID or a time; a label already on another version is refused at
  write time, and two versions merged with the same label are reported ambiguous by
  lookup rather than resolved silently.
- Preserve concurrent versions subject to retention, even when only one head wins;
  history remains administratively prunable, not a compliance log.
- **Provenance.** A local version's provenance is its `author` did:key and the signed state
  commit carrying the record; a did:key cannot rotate, so the record proof (commit, MST
  path, record) stays verifiable after the key is revoked, and the key's authority at the
  time is the root/admin `key` record chain. External sources add `observed` (name, 0045;
  atproto commit, 0054) and `imported` (0042) provenance; their historical identity
  evidence (DID document/key at observation time) is specified with those tasks.
- Entries are about 300 bytes and sync with the state repository's MST diffs. Pruned
  versions keep metadata until an explicit compaction tombstones them (`history.compact_after`
  is not automatic yet). History is neither permanently complete nor a compliance/append-only
  guarantee. Every record is re-evaluated with the state, so very long histories are a
  scale-track item (0050).

**Retention.**

```text
history: { keep_last: 10, hourly: 24, daily: 30, weekly: 12, monthly: 12, yearly: all,
           max_bytes?: … }
```

- The schedule uses grandfather-father-son thinning: keep the `keep_last` newest versions,
  then the newest version in each hour, day, week, month and year bucket, up to each count.
  Buckets are measured back from the newest version's timestamp, not the wall clock. Every
  member therefore derives the same retained set from the merged version tree, and nothing
  is pruned while no new versions arrive. Exactly (0047): a version's time is its TID's
  HLC; bucket `i` of a period `d` (1 h, 24 h, 7 d, 30 d, 365 d) holds versions with
  `floor((newest − t) / d) = i`, and the newest version of buckets `0 … count−1` is
  selected (`-1` = `all`: every bucket). The archive preset writes `{keep_last: 10,
  hourly: 24, daily: 30, weekly: 12, monthly: 12, yearly: -1}` into the policy (visible,
  provisional until pilot data, 0034). The **live set** is the schedule's selection plus
  labeled versions plus the current head's version; `max_bytes` then drops the oldest
  unlabeled, non-head selected versions while the sum of the live versions' `newBytes`
  exceeds it, and reports protected excess instead of dropping more. Its identity is the
  CID of the DRISL `{$type: "io.ipfs.nu.cluster.liveset", name, versions: [[tid, root]…]}`
  in TID order, the `live` binding of reports.
- The current served head, the last policy-ready head and labeled versions retain content,
  not just metadata. A known incoming head selected by retention remains an acquisition
  obligation only while it remains selected by the current schedule or an explicit label.
  Superseded never-completed versions outside retention can be skipped with that status;
  repeated `keep_last` reevaluation does not protect every observed head indefinitely.
  Labeling unavailable content reports absence and requires explicit reacquisition intent,
  not an instant retained-complete claim. `max_bytes` may prune eligible oldest unlabeled
  versions, but never overrides protected/labeled content. Excess protected bytes pause acquisition and warn. Logical
  quotas, physical budgets and transfer limits follow [capacity contracts](operations.md#capacity-and-transfer-budgets).
- With thinning, a dataset that publishes every minute retains a few hundred versions. At the
  D19 target, that is on the order of a GB of structure plus the changed blobs.

**Storage and pruning.**

- Every eligible storage member holds the structure of all retained versions; shared MST
  nodes are stored once. Blob placement is unchanged: a blob retained in 50 versions still
  has only `k` owners.
- **Span index:** for each blob in a dataset, members keep the version spans `[added, removed)`
  during which some path referenced it. Applying a version's diff against its predecessor
  updates only changed entries. A late version from a concurrent writer is spliced in by
  diffing it against both neighbors.
- **Pruning** version v, with retained neighbors p (older) and n (newer), can free only blobs
  in v that are in neither p nor n. Two MST diffs find these candidates. Each candidate is
  released only if the span index shows no other retained version in this or any other
  dataset. Released blobs go to GC after `dataset.remove_grace`, as for removed datasets.
- Archive-wide readiness in epoch E requires feasible copies/domains and fresh completeness
  confirmations for the retention live set bound to its version-tree root and policy, as
  in [status](#status). A newly observed incomplete version can block archive-wide readiness
  without erasing the separate availability result for a previously complete snapshot.

**Use.**

```text
nu cluster dataset versions <ds>                  # tid, time, author, note, files, newBytes, label
nu get <ds>@<tid|label|2025-01-31T12:00Z>/path     # a time selects the newest version at or before it
nu cluster dataset diff <ds> <v1> <v2>            # MST diff: paths added, removed and changed
nu cluster dataset label <ds> <v> release-3       # label, exempting the version from pruning
nu cluster dataset rollback <ds> <v>              # append restored head; existing snapshot identity unchanged
```

- Available retained-complete versions can be read without reacquisition. A rollback
  appends a new head/version with `restoredFrom`; metadata acceptance and readiness of
  that head in the current epoch are separate results. Incomplete/pruned versions require
  recovery from a provider/export or fail with missing-content status, not false immediate
  success. Concurrent head writes still resolve by LWW.
- **Availability (0047)**, per version as this node sees it, reported apart from
  readiness: `retained-complete` (content complete: locally on a one-node cluster;
  in a networked cluster once an archive-ready evaluation covered the version, or locally
  complete), `fetching` (live, being acquired; a pause carries its reason), `observed`
  (live, nothing acquired here yet), `degraded` (established complete, but a fresh owner
  report lists it incomplete or its content is no longer complete here), `unknown`
  (established, owner evidence stale), `skipped` (not live and never completed here: its
  partial acquisition is abandoned) and `pruned` (metadata only: released by `prune`).
  A version outside the live set whose content is still complete stays `retained-complete`
  with `live: false` (a release candidate) until pruned. Reads, rollback and gateway
  lookups of a version that is not `retained-complete` fail with `unavailable` naming the
  availability, and never fall back to another version. Labeling a version whose content
  is not complete requires `--reacquire`, which makes it live (an acquisition obligation)
  and reports it `fetching`/`observed` until complete.
- **Pruning (0047)**: `nu dataset prune <ds> [--dry-run]` lists versions outside the live
  set whose content is still pinned, with the bytes releasing them would free (objects no
  other pin reaches, so content shared with live versions, other datasets or explicit
  pins is reported as shared and kept) and applies only with explicit intent. On a
  networked cluster it is dry-run only until 0048 (S8). The current head, last ready,
  held and staged targets and every live root are never pruned.
- Inside the cluster, any retained version can serve as the `have` root for `/nu/tree/1`
  diffs. For datasets with history, this replaces D19's `retain_structure`.
- The gateway serves `/c/<dataset>@<version>/<path>`.

**Name sources.** For a dataset following an external name, admin daemons append each
verified target they observe, with the proof's `rev` and commit CID in `observed`, and keep
the proof blocks. This is observed history: versions superseded before any member saw them
are missing. It archives observed public snapshots, not a complete publication history.
Preserve verification provenance and specify historical signing-key/identity evidence before
claiming proofs remain verifiable after key/account changes.

### Placement

- **Structure** (directory and MST nodes, entry metadata, file nodes, tree header) of every
  placed head goes to every eligible storage member. At roughly 0.1–1% of bytes
  ([D19](follow.md#design-target)), this lets any eligible member enumerate blobs, compute
  placement, and serve tree requests and diffs. Collections narrow this rule to the
  [catalog](collections.md#items-and-facets). Item-internal structure goes only to members
  that hold the item.
- **Blobs** use weighted rendezvous hashing. For blob digest `b` and member `p`, compute
  `u = (uint64(blake3("nu.place\0" ‖ peerId_p ‖ b)[0:8]) + 1) / (2^64 + 1)` and
  `score = −capacity_p / ln(u)`. The owners are the `k` highest-scoring eligible members in
  state `active`. Prefer distinct `spread` values, then fill up to `k` with best remaining
  eligible members to preserve useful copies. Exact encoding (vectors:
  `testdata/vectors/nu-cluster/placement.json`): `b` is the blob's 34-byte binary
  multihash, `peerId_p` the PeerId bytes, the 8 bytes are read big-endian, and `u` and
  `score` are evaluated in IEEE-754 float64 (so the denominator is 2^64; `u` is clamped
  below 1). The weight is `capacity`, or 1 TiB when the record has none (0). Ties rank by
  PeerId bytes, ascending. *Eligible* means role `storage`, state `active` and every
  `where` tag present with an allowed value; `replicas: all` makes `k` the eligible count.
  With `spread`, the first pass takes, in rank order, the best member of each distinct
  non-empty value of the tag (untagged members have none). Readiness counts confirmed
  copies per blob against these owners ([E6](operations.md#e-readiness-evidence)); a
  target without blobs counts its own multihash as its one blob, so an empty tree still
  needs confirming members. A collection item is placed as one unit, keyed by the
  multihash of the item root ([floor placement](collections.md#guaranteed-copies); vectors
  in the same file, `items`). Duplicate domains do not satisfy strict
  spread: report policy-unsatisfied, not ready. Fewer than `k` members likewise cannot meet
  policy. Defaults are stored in dataset policy, not recomputed when tags change; preview
  tag/weight/member edits for feasibility and movement impacts.
- Scores do not depend on the dataset. A blob shared by datasets with the same eligible set
  therefore has the same owners and is stored once. A blob referenced by several datasets is
  owned by the union of their owners.
- Only `member` records affect placement. Adding or removing a member moves roughly that
  member's share, not the whole cluster. The **placement epoch** is a hash of the eligible
  members with their capacities, tags and states; completeness and status are keyed by it.
  It is the hex SHA-256 of `"nu.cluster.epoch.v1\0"` ‖ the DRISL array of `{peer: <PeerId
  bytes>, capacity, tags, state}` of every non-removed member with the `storage` role,
  sorted by PeerId bytes (one epoch per cluster, so any storage member change re-binds
  every dataset; the local cluster's epoch is `local`). The **policy hash** is the hex
  SHA-256 of the DRISL `policy` map exactly as signed, unknown fields included.
- No per-blob allocation is stored or agreed on. Each member determines its own blobs while
  walking structure. An epoch change triggers a rescan: about 10⁹ short hashes for 10M
  entries × 100 members. Benchmarks must confirm the cost.
- **Capacity** is an admin-set placement weight; `store_limit` is a separate physical budget
  (the member's `store.limit`, [D17 A5](data.md#accounting-and-budgets)).
  A member that reaches its store limit stops
  acquiring blobs and reports `full`, and affected datasets report under-replication.
  Placement never spills automatically, which keeps it a pure function of signed state.
  Preflight/status explain blocked owners even if other nodes have free space, and guide
  an admin through weight/quota/membership adjustment. Logical/physical/transfer accounting
  and protected-content behavior follow [operations](operations.md#capacity-and-transfer-budgets).
- All `k` owners of a large blob hold the whole blob. **Later:** place stripes of very large
  blobs by `(b, stripe index)`, because Bao ranges verify independently.

### Sync, handoff and repair

- Each storage member runs the D19 sync for every dataset for which it is eligible, fetching
  only blobs it owns. A completeness mark per `(subtree, epoch)` means that the structure and
  all owned blobs are present. (0046: the head's `nu-tree` pin under source `cluster`, name
  `dataset/<name>`, plus a completeness walk of the structure and owned blobs in the current
  epoch; `/nu/tree/1` then inlines no blobs, so a member never receives blobs it does not
  own. Under S5 a member that never saw the dataset ready works on the newest of at most
  `staging.max_heads` staged heads; when one becomes fully held, older staged heads that
  were never held are unpinned, while held and ready versions stay pinned and, with
  `release.auto` off, are listed as release candidates. Storage refusals pause acquisition
  with `insufficient_storage` and the bytes needed; nothing is unpinned.)
- Blob sources are tried in this order: owners by rank, members holding surplus copies, then
  other members found with `/nu/have/1`. There is no DHT lookup.
- **Handoff:** a blob that a member held but no longer owns becomes **surplus**. Keep its
  `cluster-surplus` pin until [S1–S3](operations.md#s-safe-release) hold for that blob:
  `k` fresh confirmed copies on its owners in the current epoch, bound to the current
  head, policy and epoch, after `release.settle`. A sampled audit is not a
  completion report (E9). Surplus holders serve new owners; unknown/partitioned evidence
  preserves their pins. `/nu/have/1` answers are not evidence.
- **Liveness and repair:** members send signed heartbeats. Admin daemons running with
  `repair = auto` mark a member `unavailable` after `repair.down_after` (default 15 min) of
  missed heartbeats, and `active` again after `repair.up_after` of resumed heartbeats. These
  daemons should run on always-on members, with at least two recommended. Excluding the
  member from placement makes the next-ranked members acquire its blobs. When it returns,
  handoff removes the temporary extra copies. Without an online admin daemon, nothing is
  re-placed automatically, and status reports the reduced copy count.
- **Draining:** a `draining` member owns nothing but keeps serving. `nu cluster member rm`
  refuses, unless forced, until the member has no unconfirmed surplus.
- **Scrub and audit:** members periodically verify stored blobs against their outboards. They
  also challenge owners for random 16 KiB groups of owned blobs. The challenger can verify the
  answer from Bao proofs against the blob CID without holding the blob. Failures are reported;
  with automatic repair, repeated failures mark the member `unavailable`. Audits show that a
  member can supply the data in time, not that it stores the data locally.

### Status

- **Heartbeats**, signed by the member's node key every 30 s:
  `{peer, seq, time, free, used, epoch, datasets: [<report>]}`. Dataset lists are sent as
  deltas against the previous heartbeat, and full status is fetched on demand. Encoding
  (0046): `reports: {base, seq, list?: [<report>], gone?: [<name>]}`. Each report's `seq` is
  `max(last + 1, now in µs)`, so it grows across restarts; `seq` is the member's greatest,
  `base` its value at the previous heartbeat. A receiver holding the sender's set at `base`
  applies the delta (`base: 0` replaces the whole set) and treats every report of that
  member as received now (E4); any other receiver, or `base: -1` (a delta over 48 KiB),
  pulls the full set with `reports`. Heartbeats are sent early when reports change, and
  carry `peers: [{peer, addrs}]` (at most 32 connected members, 4 addresses each) so members
  whose records carry no addresses, such as pre-authorized ones, can dial each other. For
  collections, heartbeats carry only each item holdings log's latest `seq`; item holdings
  are streamed separately ([D22](collections.md#holdings-reports-and-index)).
- **Completion report** ([E2](operations.md#e-readiness-evidence)), one per dataset the
  member is eligible for:
  `{name, target, live?: <version-tree root>, mode: "full"|"scoped:<path>"|"lazy"|"owned",
  policy: <hash>, epoch, complete: bool, missing?: {blobs, bytes}, owned: {blobs, bytes},
  state: "syncing"|"held"|"paused"|"error", seq}`. `target` is the exact head CID the
  member is working toward; `complete: true` is set only from completeness marks for
  `(target, epoch)` and cleared by any scrub failure or deletion. A member that holds
  every blob (of the target and every live version) reports mode `full`, which confirms
  every blob; otherwise `owned`, which confirms the blobs it owns in `epoch`. For a dataset with
  history, `live` is the live-set identity and `incomplete: [<tid>]` (at most 256) lists
  the live versions whose structure or owned blobs this member lacks; the live set is
  complete when `complete` is true and `incomplete` is empty (0047).
  `policy` is the SHA-256 of the DRISL-encoded `policy` map of the dataset record; `epoch`
  is the placement epoch hash. The heartbeat's `seq` orders reports per member.
- A dataset is **ready** at head H in epoch E by [E5–E6](operations.md#e-readiness-evidence):
  feasible policy and at least `k` confirmed copies of every blob of H: fresh
  `complete: true` reports bound to H, policy and E, from owners of the blob (`owned`) or
  from members holding every blob (`full`). The evaluator walks H's structure once,
  groups blobs by owner set and counts each group's confirming members, so a member that
  is briefly stale does not block readiness while other members confirm its blobs `k`
  times; status shows `copies: {unit, units, below, min}`. Archive-wide readiness applies
  the same rule over the blobs of every live version with `live` set to the
  retention live set's version-tree root; a snapshot's availability is reported
  separately. Reports are trusted completeness assertions, not byzantine storage proofs.
  Stale reports make the result unknown ([E8](operations.md#e-readiness-evidence)),
  never a green result because all remaining nodes are complete.
- **Release algorithm** ([S1–S8](operations.md#s-safe-release)), run per member on its
  own state: for each candidate blob b, compute `owners(b, E)` from merged state; require
  at least `k` owners other than this member with a fresh report with `target = H`,
  `live = L`, `policy = P`, `epoch = E`, `complete: true` (non-owner copies never permit a
  release, S2); require `(H, L, P, E)` unchanged in merged state for `release.settle`
  with no held-back records and no owner reporting another epoch; then move b's pin to
  GC after `dataset.remove_grace`. With `release.auto = false` (default) the decision is
  only recorded for `nu cluster dataset gc --dry-run`.
- `nu cluster status` shows observed, syncing and served roots, mode/retention, exact-target
  readiness with the [W3](operations.md#w-exact-target-waits) fields (`confirmed`,
  `required`, `missing`, `extra`, `evidence_age`, `epoch`, `policy`, `unmet`),
  replication from → to and progress, under-replicated, unknown, paused or error.
  Report missing members/domains, evidence age, capacity and unavailable repair authority,
  with a next action. CLI/API waits and alerts follow [operations](operations.md#readiness-and-status).
  It also shows each member's last heartbeat, free space, clock skew and lag. `nu cluster
  where <cid>` computes the owners of a blob and queries them. Metrics are exported to
  Prometheus.

### Membership and access

```text
nu cluster invite [--role storage|client] [--tags zone=eu-1] [--capacity 8T]
                  [--peer <peerId>] [--expires 24h]
  → nuc1…  (cluster ID, genesis, root/admin PUBLIC verification keys, bootstrap addresses,
            and either a pre-authorized PeerId or a one-time secret; never authority private keys)
nu cluster join nuc1…
```

- **Pre-authorized invites** write the member record immediately. They work offline and suit
  provisioning of air-gapped machines.
- **Secret invites** write an `invite` record. The joiner proves knowledge of the secret, bound
  to its PeerId, to a bootstrap member. Any online admin daemon then writes the member record
  and a tombstone for the invite.
- **Roles:** `storage` members receive placement. `client` members may fetch and serve but
  receive no placement; examples are laptops, applications and admin workstations. In public
  mode, a member may also run `nu relay` for others, as in D10.
- **Trust:** members are trusted to store and serve data. There is no read access control
  between members and no confidentiality from them. OS/disk protections are the initial
  boundary; content encryption and finer read permissions are later work. Private HTTP
  gateways require their own reader restrictions; libp2p gating is not HTTP authentication.

#### Invites and join

- **Token:** `nuc1` followed by lowercase unpadded RFC 4648 base32 of `T ‖ SHA-256(T)[0:4]`,
  where `T` (at most 4 KiB) is the DRISL map `{v: 1, genesis: <genesis record>, keys:
  [<admin did:key>], boot: [{peer: <PeerId bytes>, addrs: [<multiaddr>]}], expires:
  <datetime>, peer?: <PeerId bytes>, invite?: {id, secret: <32 bytes>}}` with exactly one of
  `peer` (pre-authorized) and `invite` (secret), 1–8 `boot` entries of at most 8 addresses
  each. The cluster ID is the CID of the DRISL `genesis` map. A token carries public keys,
  addresses and at most one admission secret, never an authority private key.
- **Secret proof:** `seed = SHA-256("nu.cluster.invite.v1\0" ‖ secret)` is an Ed25519 seed;
  the invite record's `proofKey` is its public key. The joiner proves the secret with the
  Ed25519 signature by that key over `"nu.cluster.join.v1\0" ‖ cluster CID bytes ‖ joiner
  PeerId bytes`. The admitting member verifies it for the TLS-authenticated PeerId, so a
  captured proof admits no other peer and no member learns the secret. Vectors for the HLC,
  genesis/cluster ID, proof and both token kinds: `testdata/vectors/nu-cluster/vectors.json`.
- **Expiry:** an invite admits only while `expires` lies ahead on the admitting member's
  clock; joiners refuse expired tokens up front. For a pre-authorized invite `expires`
  bounds only the token, because its member record exists from the start.
- **Join:** the joiner persists the token's genesis, admin keys and boot addresses (all
  public) as its cluster configuration and keeps the secret in memory only for the
  exchange. With a pre-authorized token it simply connects to boot peers and syncs. With a
  secret it sends `join` to a boot peer. A boot member holding an admin key writes
  `member/<peer>` (roles, tags and capacity from the invite, addresses from the request,
  `active`) and the invite's tombstone in one commit; otherwise it forwards `admit` to
  connected members until one holding an admin key accepts. The answer comes after the
  commit, then the joiner syncs like any member. During a partition, concurrent uses can
  admit more than one peer; each admission is a visible member record.
- **Revocation:** `member rm` writes `state: removed`; on merge, members close its
  connections and refuse it. An invite is revoked by its tombstone, a key by `revoked`
  naming its last accepted commit (above). All three work offline at the writing admin
  and take effect wherever the state replicates.
- **Gating:** private and isolated nodes accept and dial only PeerIds with a non-removed
  member record, plus the boot peers while joining. While an unexpired secret invite exists,
  inbound connections from other peers are accepted, but such a peer can open only
  `/nu/cluster/1` streams carrying `join`, and the connection closes after the exchange. In
  public mode the same stream rule covers `/nu/cluster/1` only.

#### `/nu/cluster/1`

One request per stream: a uvarint-framed DRISL map of at most 64 KiB, `{op, cluster: <cluster
ID link>, …}`, answered by `{status, error?, …}` (statuses of `/nu/bytes/1`, plus 6 for a
request a non-member or non-admin may not make). Requests naming another cluster get status 3.

- `heads {heads: [{did, rev, commit}]}` (at most 1024 entries) answers with the responder's
  heads. Members send it when a connection to a member opens, after every change to their
  held state (pushing to all connected members, the watchers), and every heartbeat interval.
  A receiver pulls each authorized DID whose `rev` is newer than its own.
- `pull {did, since?: <data root link>, commit?: <commit link>}` answers `{status}` followed by
  a CARv1: the atproto repo diff from the `since` tree to the head (or to `commit`, for a
  revocation cutoff), or a full export when the server lacks the `since` tree. The client
  verifies it as a `getRepo?since` response against the tree it holds (at most 100k blocks).
- `heartbeat {hb: {peer, seq, time (Unix ms), free, used, addrs?, sig}}` answers `{status}`; `sig` is the
  member's node-key signature over `"nu.cluster.heartbeat.v1\0" ‖ DRISL(hb without sig)`.
  Members send one to every connected member each interval (30 s).
- `holdings {dataset, epoch?, since_seq?, snapshot?, after?, limit?}` answers `{status}`
  followed by one uvarint-framed DRISL `{epoch, entries: [{seq, path, entry?, state, src?,
  bytes?}], seq, more?, after?, resync?}` of at most 10,000 entries and 8 MiB: a member's
  collection holdings log ([D22](collections.md#holdings-reports-and-index)). Heartbeats
  carry `holdings: [{dataset, epoch, seq}]`, each collection's log position.
- `join {invite, proof, addrs?}` and, between members, `admit {invite, proof, peer, addrs?}`
  answer `{status}` once the member record is committed.
- `reports {}` answers `{status}` followed by one uvarint-framed DRISL `{seq, reports:
  [<report>]}` of at most 8 MiB: the member's current completion reports, authenticated by
  the stream.

### Network modes

The mode is fixed at genesis.

| | public | private | isolated |
|---|---|---|---|
| libp2p peers contacted | anyone | members only | members only |
| Amino DHT and provider records | per dataset `announce` | no | no |
| Relays and NAT traversal | [D10–D11c](networking.md#d10-home-relays) | none; members connect directly | none; members connect directly |
| Peer addresses | [D13](networking.md#d13-peer-address-discovery) plus member records | invite bootstrap, member records and heartbeats, optional cluster mDNS | same as private; IPs or internal DNS only |
| Name-plane HTTPS (PDS, Jetstream) | yes | only for `name` sources | no; bundles only |
| Collection item URL fetches | yes | receiving writer members, `ingest.hosts` only ([D22](collections.md#ingest)) | no |
| Default infrastructure / telemetry | configured public endpoints allowed; field telemetry opt-in | no | no |

Private mode may span sites over routed links or a VPN; isolated mode also has no egress.
Neither mode sends any traffic, even encrypted, through third parties.

- **One network per daemon in v1:** a private/isolated node does not join the public network.
  A private archive's public edge is a separate public Nu daemon/store populated by explicit
  complete/incremental export-import of selected content, then public-serving consent.
  No private authority keys go to the edge; publish under separately configured identity.
  A public-mode cluster may instead keep datasets member-only until explicit `announce`.
  Live private→public automation without bundles is an open extension, not implicit dual
  networking or zero-egress. Separate PeerIds avoid automatic identity linkage.
- **Wire:** the libp2p profile is unchanged (QUIC-v1, TLS, multistream-select), so standard
  libp2p tooling with an allowlisted key can still interoperate. libp2p private networks
  (pnet PSK) are unavailable over QUIC; go-libp2p rejects that combination. Isolation
  therefore comes from mutual authentication and connection gating. Private and isolated
  nodes accept and dial only PeerIds with non-removed member records. Joiners are admitted
  only for the join exchange on `/nu/cluster/1`. In public mode, gating covers cluster
  protocols, and local datasets are served only to members unless `announce` is set.
- **Exposure:** a non-member that reaches a listening port can complete the server's half of
  the TLS handshake and learn its PeerId, but cannot open streams. Deployments needing
  concealment rely on network isolation, which isolated mode assumes, or a VPN.
- **Connectivity:** private and isolated modes assume no NAT between members. Storage members
  must be directly reachable from each other on one UDP port (default 4171), over a flat or routed network,
  a VPN, or public addresses restricted by firewall. Clients need only outbound reachability,
  because members answer on the connection a client opens. These modes do not run relays,
  circuit reservations, AutoNAT, port mapping, nu-path or hole punching. Members dial the
  addresses in member records and heartbeats. `nu cluster status` and `nu doctor` report a
  pair of members that cannot connect as a configuration error, not a condition to work
  around. mDNS uses a service name derived from the cluster ID and can be disabled.
- **Egress safety:** at startup, isolated mode refuses configuration that names a non-member
  endpoint, including bootstrap, relay, lookup, PDS or telemetry endpoints, and compiled-in
  defaults are inactive. Private mode applies the same check to everything except the
  name-plane hosts of its `name` sources and, on members receiving collection pushes, the
  dataset's `ingest.hosts`. CI runs an isolated cluster in network namespaces
  with no default route. A packet capture must show no packets to non-member addresses and no
  DNS queries for public names.
- **Clocks:** HLC ordering tolerates skew. Heartbeats measure it, and status warns above
  `clock.warn_skew` (default 30 s).

### Air-gap bundles

```text
nu cluster export <ds> [--since <root>] [--history] -o update.nub
nu cluster import update.nub [--as <ds>]
```

- A bundle is an ordered CAR whose header carries a MASL-compatible manifest under
  `io.ipfs.nu` ([D21](dasl.md#car)). The body starts with the signed state-repository
  commit plus an inclusion path for the head record, or the atproto proof for a `name`
  source. Structure nodes new since `--since` follow (or the whole tree if that option is
  omitted), then blobs. Import verifies every block and rebuilds outboards. `--history`
  also carries the version tree and the new content of retained versions after `--since`.
- Import applies the head if a key authorized in the cluster signed it and it is newer.
  Otherwise, import stores the content, and an admin must assign it with `--as`.
- Isolated clusters cannot resolve DIDs. A `name` source for such a cluster therefore pins
  the publisher's atproto signing key when it is configured. Bundles carrying newer proofs
  signed by that key advance the dataset, and key rotation requires an admin to pin the new
  key.
- Exporting an incremental bundle needs a baseline root, not receiver feedback, so it can
  traverse a one-way link. Import must preflight that the receiver actually holds required
  baseline content; report missing CIDs and preserve its served version if not. Verification
  of metadata alone is not a usable import. Complete exports/recovery precede advanced
  air-gap use. Bundles do not contain private authority keys or replace key/state backups.

### Public mode

- With `announce`, members provide the dataset's [swarm key](follow.md#dataset-swarm) and
  current root, serve its content to anyone and join its watch mesh.
- `publish` requires explicit public-serving consent (`announce` in this design); reject
  publish-with-member-only-serving rather than silently change access. A designated member
  with atproto login writes `nu.name` only after exact-head policy readiness. Outside
  followers follow it as in D19; members serve the blobs they own. Public history retrieval
  exposes only explicitly retained/available versions under the serving policy.
- Collaborative replication by outsiders, formerly the role of `ipfs-cluster-follow`, is
  plain `nu follow` of published datasets. Followers are not members and do not affect
  placement.

### Interfaces

```text
nu cluster init | invite | join | leave | status
nu cluster member ls | set <peer> [--capacity --store-limit --tags --state] | drain <peer> | rm <peer>
nu cluster key new | grant <did> admin|writer [--dataset <ds>] | revoke <did>
nu cluster dataset create <ds> [--replicas k|all] [--where …] [--spread zone] [--source at://…]
                           [--history daily=30,monthly=12]
nu cluster dataset add | edit | update | ls | status | rm
nu cluster dataset versions | diff | label | rollback <ds> …
nu cluster pin <cid> [--name ds]
nu cluster where <cid>
nu cluster export | import
```

The local API adds `/v1/cluster/…`. The gateway serves cluster datasets at
`/c/<dataset>[@<version>]/<path>`. `nu dataset …` and `/v1/datasets/…` are the same commands
and endpoints applied to the [local cluster](#d20-clusters).

### Out of scope

Byzantine members, storage proofs beyond audits, incentives, erasure coding, linearizable
operations and multi-cluster federation, except through bundles and public names, are out of
scope. Importing IPFS Cluster/Kubo pinsets follows the [migration contract](migration.md).
Transient over-replication is expected. `k` fresh confirmed copies is a readiness condition,
not a guarantee that failures never reduce available copies. [Operations](operations.md)
require key/state backup, root-compromise recovery, destructive dry runs and clean-machine
restore before a production recommendation.

## Details still to specify

- Evaluation cost on large clusters: per-blob counting walks the target's structure and
  hashes each blob against every eligible member per evaluation (status, waits);
  caching owner groups per `(target, epoch)` is scale-track work (0050/0068).
- Immutable version identity/collision validation and annotation authority; import manifest
  references authenticated by signed versions, plus historical key/identity evidence.
- Mirror/archive preset defaults, historical availability, provenance/key evidence, budgets,
  capacity adjustment, conflict reconciliation and key/state recovery/export procedures.
- Version tree encoding, span-index storage cost at 10M entries × hundreds of versions,
  exact bucket boundaries for the retention schedule, and `compact_after` defaults.
- `/nu/cluster/1` mesh degree once full mesh stops scaling.
- Tombstone compaction.
- Cross-implementation placement vectors beyond Go's float64 `math.Log` (the encoding and
  `spread` selection are specified above), rescan benchmarks, per-dataset epochs, and
  whether capacity in bytes is the right weight.
- Storage cost and invalidation of per-epoch completeness marks.
- `down_after`/`up_after` defaults, flap damping, and handoff when an owner is `full`.
- Bundle size limits and splitting, and atproto proof verification against a pinned key.
- Audit rate, deadlines and failure thresholds.

## References

- [IPFS Cluster consensus components](https://ipfscluster.io/documentation/guides/consensus/)
  and [collaborative clusters](https://ipfscluster.io/documentation/collaborative/)
- [libp2p private networks (PSK)](https://github.com/libp2p/specs/blob/master/pnet/Private-Networks-PSK-V1.md)
- Rendezvous hashing: Thaler & Ravishankar, "Using name-based mappings to increase hit rates"
  (1998); weighted variant: Schindelhauer & Schomaker (2005)
- Hybrid logical clocks: Kulkarni et al., "Logical Physical Clocks" (2014)
- [atproto repository format](https://atproto.com/specs/repository)
