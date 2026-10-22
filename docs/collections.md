# Collections

[Spec overview](../spec.md) · [Data](data.md) · [Following](follow.md) · [Clusters](cluster.md) · [Operations](operations.md) · [Validation](validation.md)

A **collection** is a dataset made of many immutable **items** that keeps growing: satellite
scenes, instrument captures, catalogued records with attached files. Each item has a small
metadata record. Collaborating operators each decide by metadata which items their nodes
hold, and everyone can see how many copies of each item exist. The motivating workload is
the [decentralized Sentinel Collaborative Ground Segment pilot](validation.md#pilot-candidates):
tens of independently operated nodes, tens of thousands of new scenes per day, regional
and rolling holdings, and spare-capacity nodes that rescue under-replicated scenes.

Collections reuse the tree, store, transfer, cluster-state and follow machinery. They add
item metadata ([D3](data.md#d3-directory-trees)), holdings chosen by members, union heads
for several ingest writers, an item push API for producers, and item-level holdings
reports.

"Collection" here has the STAC sense. atproto record collections are always called NSIDs
in Nu's interfaces (for example the `atrepo` source's `nsids` filter).

## D22. Collections

### Design target

These are sizing targets, not measured limits; [validation](validation.md) owns pilot limits.

| Quantity | Target |
|---|---|
| Ingest | 20k+ new items/day per collection, sustained, from several writers; about 20 TiB/day at full Sentinel-2 L2A size |
| Items | 10M per collection (the [D19 target](follow.md#design-target)); shard by time beyond that |
| Item | a directory of tens of files, typically about 1 GiB and up to several GiB (a full L2A scene is 19 COG/metadata files, 1.05 GiB); metadata typically 1–2 KiB, at most 64 KiB |
| Members | tens of storage members, each with its own selection |
| Visibility | copies per item, rarest items, holders of an item, items of a member, transfers; fresh within two heartbeats |

Every member holds the catalog, which costs about 2 KiB per item including tree overhead.
That is roughly 40 MB/day at 20k items/day, and about 20 GB for 10M items. The internal
structure of an item (tens of entries) is held only with the item. A member that keeps every
item of a 20k/day collection needs about 2 Gbit/s sustained just to keep up. Benchmarks
must confirm these figures before they become supported limits.

### Items and facets

- An **item** is a tree entry that carries `meta` ([D3](data.md#d3-directory-trees)), usually a
  directory of files. Entries below an item belong to it, and items do not nest. The writer
  chooses the path layout. Time-prefixed paths such as `2026/10/19/<item-id>` keep path
  scopes, listings and expiry cheap, and keep different writers' trees sharing all but their
  newest subtrees. Item paths should derive from a stable source identifier (e.g. the STAC
  id), so that independent writers of the same item choose the same path.
- **Metadata records** are application DRISL (generic DRISL, so floats are allowed) of at
  most 64 KiB. They are structure: SHA-256 and stored in the DB.
- The **catalog** is the structure above and including the item entries: directory MSTs
  down to each item's entry, plus its metadata record. Every eligible member holds the
  catalog, which replaces D20's rule that every eligible member holds all structure. Any
  member can therefore list and query every item without fetching item contents. An item's
  **internal structure** (its directory MST and file nodes) is held only by members that
  hold the item, either self-selected or through placement.
- The tree header declares **facets**, which are typed values extracted from metadata by
  JSON Pointer over the atproto-style JSON form ([D21](dasl.md#drisl)):

  ```text
  collection: { facets: {
    sensed:   { ptr: "/stac/properties/datetime",       type: "time" },
    bbox:     { ptr: "/stac/bbox",                      type: "bbox" },
    cloud:    { ptr: "/stac/properties/eo:cloud_cover", type: "number" },
    platform: { ptr: "/stac/properties/platform",       type: "string" } } }
  ```

  Types are `time` (RFC 3339 string or integer milliseconds), `number` (integer or float),
  `string` and `bbox` (`[west, south, east, north]` in degrees; west > east crosses the
  antimeridian). A missing or ill-typed value leaves the facet absent, and predicates on an
  absent facet are false; status counts such items per facet.
- Members index facets locally as they apply heads. Facets are the writer's claims about
  an item, not verified properties of its bytes.

### Selection

Admins decide membership and guaranteed placement. Operators decide what else their nodes
keep. Each storage member, and each [follower](#following-collections), may hold items chosen
by rules its operator configures locally. Rules are node state, not signed cluster
state, and an admin cannot override them. The operator sets them per collection with
`nu cluster collection rules <ds> --set rules.toml` (or `PUT …/rules`); the node keeps
them in its collection database.

```toml
[[rule]]        # alpine scenes, mostly clear, for 48 h
bbox   = { intersects = [5.9, 45.8, 10.5, 47.8] }
cloud  = { max = 30 }
sensed = { within = "48h" }

[[rule]]        # spare capacity for under-replicated scenes
rescue = { below = 2, max_bytes = "100GiB" }
```

- Rules are evaluated in order, and the first matching rule decides. A rule matches when all
  its predicates hold: `eq`/`in` for strings, `min`/`max` for numbers and times, `within`
  (a duration back from now) for times, and `intersects` for bboxes. `hold = false` excludes
  items. Unmatched items are not held. Rules may also set `path` and `max_bytes`.
- **Partial items:** a rule may set `files`, a list of globs relative to the item (for
  example `["visual.tif", "*.json", "*.xml"]`). The member then holds only those files plus
  the item's internal structure, and reports a `partial` holding. Copy counts include only
  complete holders. Per-file copy counts are available for each item.
- **Reconciliation, not events.** Whenever a member applies a new head, it evaluates added
  and changed items. Whenever a `within` window moves, it re-evaluates the items at that
  window's edge. Holdings are therefore a function of catalog, rules and clock. Missed
  notifications, restarts and members that join late converge to the same holdings, and
  nothing depends on having seen an item arrive. Backfill after a rule change is paced by
  the transfer budget.
- **Rolling holdings:** an item that leaves a `within` window, or that a changed rule no
  longer selects, loses its `collection-select` pin. The member then sweeps the item's blobs
  and internal structure incrementally within `gc.release_delay` (default 1 min), reclaiming
  whatever no other source pins. It does not wait for GC's size watermark, because a rolling
  node frees about as much as it fetches every day. No protected-version rule applies,
  because the release is the operator's choice.
- **Budgets:** `max_bytes` bounds the logical bytes each rule holds. When it is reached, new
  matches pause and status reports the shortfall
  ([capacity](operations.md#capacity-and-transfer-budgets)). Nu never evicts selected items
  to make room; only rescue rules exchange items, as described below.
- **Rescue:** a `rescue` rule holds the items it matches whose fresh complete-holder count is
  below `below`, rarest first, within its budget. To make room it may release items it
  rescued earlier once their count reaches `below + 1`. It needs the
  [holdings index](#holdings-reports-and-index), either local or on a configured index
  member. Rescue is best effort: stale counts cause temporary over- or under-replication.
- **External deciders:** arbitrary policy code runs outside the daemon and uses the API. It
  follows an item change stream with a cursor (`GET /api/v0/cluster/collections/<ds>/items?since=`) and
  calls `hold`/`release` per item, which sets or clears pin source `collection-manual`. Nu
  runs no user code.
- Selected items are fetched first from the members whose writer heads contain the item
  (initially only the member that received the push), then from holders known from holdings
  reports or the index, then from placement owners, then from other members via
  `/nu/have/1`. Large files are split across all of these sources with Bao ranges
  ([D2](data.md#d2-blobs)), so popular new items spread as they arrive. Members that run no
  index still follow the holdings logs of the members they fetch from. Followers use the
  [dataset swarm](follow.md#dataset-swarm).
- `nu cluster collection rules <ds> --dry-run` reports the items and bytes a rule set would
  hold before it is applied.

### Guaranteed copies

- Collection policies keep D20's `replicas`, `where` and `spread`. Those copies are placed by
  [rendezvous hashing](cluster.md#placement) on the eligible members, exactly as for other
  datasets, except that the unit is the **whole item**: `b` in the placement score is the
  34-byte binary multihash of the item root (the CID of the item's entry in the catalog,
  for a directory item its SHA-256 DRISL directory node), so the `k` floor owners of an
  item hold all of it and nothing about its internal structure is needed to place it.
  Vectors: `testdata/vectors/nu-cluster/placement.json`, `items`. Balance is per item, so
  coarser than per blob. Only floor copies count toward policy readiness. `replicas: 0` is
  allowed only for collections and means no guaranteed copies.
- **Floor holdings:** an eligible member holds every item it owns with source `floor`
  (pin source `cluster`), before manual holds, its own written items and rules, and with
  no rule budget. An epoch or policy change re-decides every item. A complete floor item
  that the member no longer owns, or that left the catalog, stays held as `surplus` (pin
  source `cluster-surplus`) and, with `release.auto` off, is not released; an incomplete
  one is dropped. Holdings logs and the index carry `floor` and `surplus` like other
  sources, so copy counts, rarest-first and `where` include them; `where` also names the
  item's floor owners, and collection status shows the node's floor counts.
- **Floor readiness** is [E6](operations.md#e-readiness-evidence) with items as units:
  each floor member's completion report is bound to the merged catalog root, the policy
  hash and the placement epoch, mode `owned`, `owned.blobs` counting items; `complete`
  means the last evaluation succeeded and every `floor` holding is complete. An evaluator
  groups the catalog's items by floor owners and requires `k` fresh complete reports
  among each item's owners; `dataset status` and `wait` bind to the merged root.
- Self-selected copies are additive and best effort. Status reports them as holder counts and
  never as `replicated`. A typical deployment tags a few large members (`where: role=floor`,
  `replicas: 1` or `2`) and lets every other member select.
- A member never needs permission to release a self-selected item. If the release leaves an
  item with no known complete holder, status records an event. An item without a fresh
  complete holder is reported `unheld`, and its catalog entry remains.
- Counting self-selected copies toward `replicas`, which would avoid storing duplicates on
  floor members, needs the readiness-evidence protocol of
  [operations](operations.md#readiness-and-status) and is later work.
- **D20 defaults for collections:** collections use mirror semantics with `history` off by
  default. The protected-version rule and surplus handoff apply only to items placed by
  `replicas`. Self-selected holdings follow the latest merged head without readiness gating.
  With `replicas: 0`, nothing waits on the release-evidence protocol. Archive history for a
  collection is an explicit opt-in, and its encoding is still to specify.

### Union heads

Collections ingested at several nodes would make D20's last-writer-wins head lose items.
A collection dataset therefore defaults to `merge: union`:

- Each listed writer writes `head/<name>` in its own state repository. The effective head is
  the canonical union, by path, of the latest head of every authorized, unrevoked writer.
  Admins remove items with a `drop/<name>` record that the union applies afterwards; its
  encoding is still to specify.
- The merged root is deterministic. Every member computes it incrementally by diffing each
  changed writer head against that writer's previous head. Waits and readiness bind to the
  merged root plus the vector `{writer did → head cid}`. Members fetch only the catalog
  portion of writer heads. Because MSTs are canonical, writers holding the same items under
  time-prefixed paths share every subtree except the newest, so N writers cost little more
  catalog storage than one. Members keep only each writer's latest head and the merged
  catalog.
- When writers supply the same path with identical entries, the result is one item. When the
  entries differ, the entry from the writer listed earliest in the dataset's `writers` wins,
  which is stable across later head writes. Heads written by admins not listed as writers
  rank after all listed writers, in DID order. Status reports the conflict with both entries,
  and the losing entry stays pinned as `collection-conflict` until resolved. Writers should
  produce deterministic metadata (no packaging timestamps), so that independent ingest of
  the same source item does not conflict.
- A writer removes items only from its own head, so an item stays while any writer's head
  holds it. An optional `expire: {facet, after}` dataset rule makes every writer daemon prune
  the same items from its head.
- Writers coalesce head writes to at most one per `head.min_interval` (default 10 s). These
  are cluster-local state commits, not PDS writes.

### Ingest

Producers push items to Nu, and Nu does no source discovery. A producer is any program that
finds new data and prepares it. In the CollGS pilot, the producer is the existing
torrentizer: it follows the Matadisco feed on Jetstream, fetches STAC items and runs GDAL.
Nu takes over at the point where the torrentizer would build a torrent.

**Item API.** One call adds one item to a collection:

```text
POST /v1/cluster/collections/<ds>/items
  { path, meta: {…} | <cid>,
    files: { <relative path>: <upload> | {path, nocopy?} | {url, checksum?, size?} } }
→ { path, entry?, state: accepted|fetching|written|failed, head? }

nu cluster collection put <ds> <path> --meta meta.json [<file>… | --url <name>=<url>…] [--nocopy]
```

- **Files** are keyed by their path inside the item (e.g. `R10m/B02.jp2`), so one push can
  carry a whole multi-file scene. Each is an upload, a local path on the receiving node (a
  file or directory, optionally `--nocopy` to seed in place like the prototype), or a URL.
  The node fetches URLs with [URL import](data.md#d17-store-and-replication), which is the
  equivalent of the prototype's web seed. Files become BLAKE3 blobs. A supplied checksum (a
  multihash, such as STAC `file:checksum`) is verified after download, and a mismatch fails
  the item. Without one, integrity rests on TLS. S3 multipart ETags (`…-21`) are change
  validators, not content hashes.
- **Completion:** the call returns once the push is durably queued. The member writes the
  item into its own head once every file is stored, and `?wait=written` blocks until then.
  Copies on other members are a separate question for the
  [holdings index](#holdings-reports-and-index).
- **Idempotent by path:** pushing an identical entry again is a no-op. A different entry at
  an existing path is reported as a conflict, and `replace: true` supersedes it in this
  writer's head. Producers can therefore retry after timeouts or restarts, or resend
  everything since their own cursor, without keeping a delivery ledger.
- **Failures:** URL fetches retry with backoff within the dataset's `ingest.limits`.
  Persistent failures (HTTP 4xx, size cap, checksum mismatch) mark the push `failed`. A failed
  push writes no entry. Producers read the outcome from `GET …/items/<path>` or follow the
  push log (`GET /v1/cluster/collections/<ds>/pushes?since=<cursor>`).
- **Bulk and removal:** `POST …/items:batch` takes NDJSON for backfills, and producers that
  build whole trees can still use `nu tree edit` and head writes. `DELETE …/items/<path>`
  removes an item from the receiving writer's head.
- **Authentication:** a producer on another machine uses an authenticated API credential
  scoped to item pushes for named collections
  ([API exposure](operations.md#api-gateway-and-public-exposure)).

**Receiving members.** A member that receives pushes must hold a writer key for the
collection, because its daemon writes the head. For redundancy, a producer pushes each item to
two receiving members, and [union heads](#union-heads) merge identical entries. That works only
if the producer's metadata is deterministic (no packaging timestamps) and its transforms are
byte-reproducible. Otherwise status reports conflicts, and the producer should pick one member
per item. No other member needs to know about producers.

**Egress.** URL fetches use `httpsrc`'s SSRF and redirect guards. In private mode, receiving
members may fetch only from the dataset's `ingest.hosts` patterns; URLs and redirects to other
hosts fail. Other members keep the normal private-mode rules. Isolated clusters accept uploads
and local paths only.

### Holdings reports and index

- For each collection, every storage member keeps an item holdings log of
  `{seq, path, entry, state: fetching|partial|complete|released}` transitions, bounded by default to
  1M transitions. Heartbeats carry the log's latest `seq` per collection instead of item data.
- `/nu/cluster/1` `holdings {dataset, since?}` returns the changes after a cursor, or a paged
  snapshot that ends with a cursor. A cursor older than the log answers `resync`, never an
  empty delta. Reports are authenticated by the member's session and are trusted-member
  assertions, like heartbeats.
- Any member may run the **holdings index** (`cluster.index = true`). It is recommended on one
  or two always-on members and on every member with rescue rules. The index follows every
  member's log from persisted cursors and keeps item → holders in the embedded DB.
- A holder counts toward an item's copies only while it reports the item `complete` and its
  heartbeat is fresh. Holders with stale heartbeats count as `unknown`, not as lost.
- Queries cover copies per item, rarest items, holders of an item, items and bytes per member,
  `unheld` items, facet filters and in-flight transfers with rates. They are available as
  `nu cluster collection items <ds> [--rarest | --holder <peer> | --unheld | --facet …]`,
  `nu cluster where <ds>/<item>`, `/v1/cluster/collections/<ds>/…` and Prometheus
  aggregates. A dashboard is an API client and is not part of the daemon.

### Following collections

Public followers use the same rules, with `nu follow <name> --select rules.toml`. The follow
pins the complete catalog plus the selected items; `--max-size` applies as a budget over
every rule's holdings. Rescue rules need an index and are therefore cluster-only. Followers
report holdings to no cluster; opt-in public holdings reports are later work.

Implemented (0058):

- A select follow is a collection dataset (`collection: {}`, `source.follow.select`,
  replicas 0) whose single writer is the followed name: the admin that resolves the
  name writes each verified target as the head, and the collection manager of the
  follow's scope (the local cluster's for `nu follow`, the networked cluster's for
  `--cluster`) syncs that head's catalog exactly as it syncs a member's writer head
  (`catalog/<ds>` and `catalog/<ds>/<signer>` pins under `nu-catalog`, the item stream,
  facets index). The **facets come from the published header** (`collection.facets` of
  the merged header above); the follow record declares none. The follower's own head
  signature does not make it a writer: items are held by rules only.
- Rules are the same language and node state (`rules` in the scope's collection
  database); `nu follow <name> --select` on an already followed name replaces them (and
  `--max-size`), and `nu cluster collection rules|status|items|where|hold|release <ds>`
  work on the follow's dataset. Rules set before the first catalog sync are accepted
  unchecked (the facets are not known yet) and checked afterwards; rules that do not fit
  the published facets are ignored with a status warning. `rescue` and `files` are
  refused; `--all`/`--replicas` do not combine with `--select`.
- Evaluation is the cluster member's: added and changed items on every new head, the
  edge of each `within` window, paused items, and everything after a rule change, with
  the same budgets (per rule `max_bytes`; the follow's `max_size` over all holdings;
  neither evicts a held item, new matches pause and `follow ls` reports the
  shortfall), pins (`collection-select`, `collection-manual`) and release sweep. A rule
  change or a newer publication re-evaluates from the catalog diff: held items the new
  rules keep get no new holdings-log transition and are not fetched again.
- Status: `follow ls` reports `mode: select`, **held** as the head whose catalog is held
  whole (the view `/c/<ds>` and `nu ls <ds>` serve), **syncing** as a newer head whose
  catalog is not, and `selection: {catalog, catalog_synced, facets, rules, items,
  selected, held, held_bytes, fetching, paused, shortfall, warnings}`.
- **Completeness marks for a partially selected view:** the held root is complete under
  the `nu-catalog` profile only (every container, item entry and metadata record), and
  each held item is complete under `nu-tree` by its own pin. Nothing marks the root
  complete under `nu-tree`, so `pin add`, `share`, `public add` and `get` of the whole
  view see it as incomplete and `get` fetches the missing items on demand (cached,
  unpinned); `ls` lists any held directory without fetching what is below it; `cat` of
  an unselected item's file is fetched on demand or fails `unavailable` without a
  provider. Readiness (E2) for a select follow is `ready` once the catalog is held and
  no selected item is fetching.
- Unfollow drops the follow's collection state and releases its catalog and item pins
  (`orphans` lists them); other pins stay.
- Not built: `--path` with `--select` (refused as before), proof-chain pinning, per-item
  fetch sources beyond the connected peers, transfer budgets, and public holdings
  reports.

### Interfaces

```text
nu add <path|https-url> --meta meta.json             # entry with a metadata record
nu tree edit <root> --put p=<cid> --meta p=<cid|@meta.json>
nu cluster dataset create <ds> --collection --facets facets.toml
                           [--replicas 0|k --where role=floor] [--writer <did> …] [--expire sensed=400d]
                           [--ingest-hosts <pattern>…]
nu cluster collection put <ds> <path> --meta meta.json [<file>… | --url <name>=<url>…]
nu cluster collection items | where | hold | release | conflicts <ds> …
nu cluster collection rules <ds> [--dry-run]
nu follow <name> --select rules.toml
```

### Out of scope

Source discovery (Jetstream/atproto feeds, STAC polling) and transforms, which producers
handle and then push; running user code in the daemon; spatial indexing beyond bbox
intersection; verifying metadata against item bytes beyond supplied checksums; holdings
reports from non-members; and byzantine holders.

### Encodings and semantics (preview, 0057)

These decisions are the preview contract; vectors for facets, predicates and merged
roots are in `testdata/vectors/nu-collection/vectors.json`.

- **Collection policy:** the dataset record's `collection` field is `{facets?: {<name>:
  {ptr, type}}, merge?: "union" | "lww", expire?: {facet, after}}`. It is authoritative for
  members; writers copy the facets into their tree headers' `collection` map so a head is
  self-describing. Collections default to `replicas: 0`. At most 32 facets, named
  `[a-z][a-z0-9_:.-]{0,63}` and not a rule keyword (`name`, `hold`, `path`, `max_bytes`,
  `files`, `rescue`, `note`).
- **Facets:** `ptr` is an RFC 6901 pointer (at most 256 bytes) over the metadata record's
  atproto-style JSON form, so `/x/$link` reads a link as its CID string and `/x/$bytes`
  bytes as unpadded base64. `time` is an RFC 3339 string with a time zone (fractions beyond
  milliseconds truncated) or integer Unix milliseconds, stored as integer milliseconds;
  floats are ill-typed. `number` is an integer or float, compared as a float64 (integers
  beyond 2^53 round). `string` is compared byte-exact. `bbox` is `[west, south, east,
  north]`, or the 3D `[west, south, min-z, east, north, max-z]` reduced to 2D, with
  longitudes in [-180, 180], latitudes in [-90, 90] and south <= north; anything else
  leaves the facet absent.
- **Predicates:** `eq` and `in` compare strings; `min`/`max` are inclusive and take numbers,
  or for times an RFC 3339 string or integer milliseconds; `within` holds when the time is
  at or after `now - within` (future times stay inside); `intersects` holds when the closed
  boxes overlap, either side may cross the antimeridian, and touching edges intersect.
  Durations are `<integer><unit>…` with units `ms s m h d w`; byte counts are integers or
  strings with `B`, `K`/`KiB` … `P`/`PiB` (powers of 1024) or `KB` … `PB` (powers of
  1000). A rule's `path` limits it to items at or below that path. Rules are validated
  against the facets when set; a predicate that does not fit its facet's type is an error.
  `files` (partial items) is rejected until it is implemented.
- **Holding precedence:** floor ownership, then a kept surplus copy, then a manual hold,
  then items this node wrote (an item in its own writer head is always held: the writer
  is the first source), then the first matching rule. A rescue rule's matches are held
  only by the rescue pass. Holdings are per path: a replaced entry at a path takes over
  the holding, and the previous entry is not kept as surplus.
- **Catalog:** the `nu-catalog` walk profile is the nu-tree profile except that an entry
  with `meta` contributes only its metadata record, and plain files and symlinks outside
  items are not followed. Members pin each merged writer head and the merged header with
  it and fetch catalogs with it (layer 1 only).
- **Union merge:** leaves are items and plain files or symlinks outside items. Each leaf
  path is decided on its own: walking the writers in rank order (the dataset's `writers`
  in list order, then other authorized signers in DID order; revoked keys contribute
  nothing), the first writer with anything at the path decides; a leaf there is merged,
  while a directory at the path or a leaf at a prefix of it merges nothing there. Later
  writers' different leaves at the path are `entry` conflicts; their leaves the decider
  blocks are `structure` conflicts. Paths at or below a `drop` path are left out. Merged
  directories carry no metadata and their size is the sum of their entries. Because each
  outcome depends only on the writer trees, the incremental merge (re-deciding only the
  changed leaves, their prefixes and the leaves below them) equals a merge from scratch.
  The merged header is `{v: 1, root, files: 0, bytes, collection: {facets}}`; `files` is 0
  in collection headers (item counts come from the catalog). `merge: lww` merges only the
  LWW head.
- **Drop:** `drop/<name>` is admin-written `{paths: [<item path>], hlc}` with at most 256
  paths, replaced as a whole by LWW. Admins should prune paths once writers have removed
  the items.
- **Conflicts:** listed by `nu cluster collection conflicts <ds>` and per item by `where`;
  the losing entry stays pinned (`collection-conflict`) until the conflict disappears. A
  conflict is resolved by the losing writer replacing or removing its entry (0059's
  `replace`), or by an admin drop; there is no separate resolve command.
- **Writer heads:** a writer node queues item changes durably in its collection database
  and writes them into its head, `{target: <tree header>, kind: "tree", size}`, at most
  once per `head.min_interval` (10 s). It signs with a held key listed in the dataset's
  writers (in list order), else an admin key, and pins its own head whole. `expire` makes
  it prune paths whose time facet (as the merged view extracted it) is older than
  `now - after` on every head write and pass.
- **Item stream:** each catalog change gets the next sequence number of the collection on
  this node; the stream returns, after a cursor, each changed path once with its latest
  state and marks removals. Its cursor is opaque and bound to the node's collection
  database epoch (`resync` otherwise).
- **Holdings log:** per collection on each member, `{seq, path, entry, state, src, bytes}`
  with `state` `fetching`, `complete` or `released` (`partial` is reserved) and `src`
  `select`, `rescue`, `manual`, `writer`, `floor` or `surplus`; bounded to 1M transitions; positions are
  `(epoch, seq)`, where the epoch names the member's collection database. The `holdings`
  request ([`/nu/cluster/1`](cluster.md#nucluster1)) answers the transitions after a
  cursor, `resync` for a cursor from another epoch, past the head or older than the log,
  and a snapshot paged by path whose pages carry the log position it started at.
- **Holdings index:** runs when `cluster.index = true` or any local rule rescues, following
  every member's log (this node's directly) whenever its heartbeat reports a later
  position, and keeping `(member, item) → (entry, state, src)` in the collection database.
  A holder counts as a copy while it reports `complete` and its last heartbeat arrived
  within three heartbeat intervals; complete holders with older heartbeats are `unknown`.
- **Rescue damping:** a rescue rule acquires matched items with fewer than `below` fresh
  copies, rarest first, until its budget is used. It releases a rescued item once the
  item has `below + 1` fresh copies, but only on the fresh rescue holder whose
  `SHA-256("nu.rescue\0" ‖ peer ‖ "\0" ‖ path)` is greatest, so rescuers acting on the same
  counts step back one at a time. Acquisitions are not coordinated: concurrent rescuers can
  overshoot, which the release rule trims.
- **Release sweep:** released items lose their pin; a store GC (one-hour grace for
  in-progress writes) runs `gc.release_delay` (1 min) after the first release. An
  incremental sweep is hardening work.
- **Fetch sources:** catalogs and items are fetched from connected members (in private
  mode, every reachable member); the ordered source list above (writers, then holders,
  then placement owners) is later work.

### Item pushes and URL import (preview, 0059)

These decisions are the preview contract of the push path; routes and types are in
[api.md](api.md#item-pushes-0059).

- **Accept:** a push is validated (path, metadata record, 1–4096 files, each exactly one of
  `upload`, `path` or `url`) and durably queued in the collection database before the call
  returns `202 accepted`; the metadata record is stored and every object the push refers to
  is pinned `collection-push` until the push is written or failed. Only a member holding a
  key that may write the collection's head receives pushes (`forbidden` otherwise).
  Metadata is an atproto-style JSON object (generic DRISL, floats allowed, at most 64 KiB)
  or the CID of a held record.
- **Files:** `upload` names a BLAKE3 blob stored by `POST …/uploads` (an
  `application/octet-stream` body) within the store's one-hour GC grace; `path` is an
  absolute regular file on the receiving node, copied or with `nocopy` referenced in place
  (directories are expanded by the CLI); `url` is fetched by URL import. The item is a
  directory of plain file entries (no mode or MIME type, so entries are reproducible across
  receiving members) whose entry carries the metadata record.
- **Idempotence:** a push's digest is SHA-256 over its metadata CID and, per file, the
  upload CID, local path (and `nocopy`), or checksum (else URL) with the declared size. A
  push with the digest of the path's current push answers that push with
  `duplicate: true` (a failed one is retried as a new push), so presigned URLs whose query
  changes do not defeat idempotence when a checksum is given. A different push while one
  is in progress at the path is a `conflict` (unless `replace`, which supersedes it).
- **Written:** once its files are stored the entry is compared with this node's writer
  side at the path (a queued change, else the writer head). The same entry is a no-op;
  a different one fails the push with code `conflict` (`details.existing`) unless
  `replace`; otherwise the entry is queued for the next head write. The push becomes
  `written` (with `head`, the writer head's tree header) in the same database transaction
  that clears the queued change after the head write, so a crash in between re-applies the
  change to the same head and marks it then. `?wait=written[&timeout=]` (default 30 s, at
  most 5 min; `timeout` error with `details.push` after that) waits for `written` or
  `failed`.
- **Failures:** URL files are fetched one attempt at a time by the ingest worker
  (4 pushes concurrently), with backoff from 5 s doubling to 10 min between attempts,
  until `ingest.limits.attempts` (default 6). Permanent errors fail at once: policy
  denials (unlisted host, redirect to one), HTTP 4xx other than 408/416/429, size over
  `ingest.limits.max_bytes` (default 1 TiB per file) or the declared `size`, a checksum
  mismatch (which also demotes the final origin), a missing local file or upload. Each
  attempt is bounded by `ingest.limits.timeout` (default 2 h) and a 1 min network idle
  timeout. A failed push writes nothing and releases its pins.
- **Resume:** a URL file's retained bytes live in `cluster/net/ingest/` beside the
  collection database with the response validators, so a member restart resumes the
  download with `Range`/`If-Range` on a strong ETag (or a qualifying Last-Modified);
  without one it restarts. These bytes are not yet charged to `store.limit`.
- **Checksums:** hex multihashes (STAC `file:checksum`): sha2-256, sha2-512 and blake3;
  others are `invalid_argument`. Without a checksum, integrity rests on TLS.
- **Removal:** `DELETE …/items/<path>` queues the path's removal from this node's writer
  head (idempotent; it cancels a push in progress there) and answers state `removed`;
  `head` is set once a head write no longer has the item.
- **Push log:** the pushes table is the log: each pushed path appears once with its latest
  state, in the order of its last change, after a cursor (`since=0` from the start;
  `next_cursor` always set; a cursor from another collection-database epoch answers
  `resync`). Retention equals the item table's: one row per path ever pushed.
- **Batch:** `POST …/items:batch` takes `application/x-ndjson`, one push request per line
  (at most 10 000 lines, 1 MiB each), and answers NDJSON
  `{line, path, push | error}` in request order; each line is accepted or rejected on its
  own.
- **Egress:** `ingest.hosts` holds exact host names or IP literals (httpsrc's allowlist
  rules: no wildcards or suffixes). In private mode receiving members fetch only from them
  (none when unset); in public mode they narrow fetches when set; isolated members refuse
  URL files. URLs are checked at accept, and every redirect before any DNS lookup or dial.
  Plain HTTP and private source addresses (a producer's LAN web seed) need trusted node
  config: `ingest.allow_http = true` and `ingest.allow_ips = ["10.0.0.0/24"]`.
- **Credentials:** remote producers use the producer listener with a push token scoped to
  named collections ([A8](operations.md#a-local-api), [api.md](api.md#producer-listener-and-push-tokens)).
  Per-producer rate limits are not implemented.

## Details still to specify

- Per-producer rate limits and quotas; charging in-progress URL downloads to
  `store.limit`; TLS on the producer listener.
- Partial items (`files`), per-file copy counts and `partial` holdings.
- Union-head history (which entries a writer appends for merged versions); collections
  run with `history` off.
- A remote holdings index (rescue on a member that does not run the index itself), and
  index storage and query cost at 10M items × tens of members: the preview scans the
  catalog for queries and status. Measured catalog bytes per item, index cost and
  re-evaluation cost are hardening evidence (0068).
- Catalog growth and expiry defaults (no expiry by default). Atomic switches of a held
  view between catalogs (a select follow moves its held root as soon as the new catalog
  is held, before its newly selected items are) and partial-item completeness, shared
  with D19 scopes.

## References

- [Decentralized Sentinel Collaborative Ground Segment](https://hackmd.io/@vmx/decentralized-collgs-202609)
  and [implementation notes](https://hackmd.io/@vmx/decentralized-collgs-implementation-202609)
- [collab-cluster-experiment](https://github.com/vmx/collab-cluster-experiment) and
  [collab-cluster-utils](https://github.com/vmx/collab-cluster-utils) (data-manager, rescuer)
- [Matadisco](https://matadisco.org/), [STAC](https://stacspec.org/),
  [STAC file extension (`file:checksum`)](https://github.com/stac-extensions/file)
- [JSON Pointer, RFC 6901](https://www.rfc-editor.org/rfc/rfc6901)
