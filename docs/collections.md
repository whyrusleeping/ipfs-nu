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
by rules its operator configures locally. Rules are node configuration, not signed cluster
state, and an admin cannot override them.

```toml
[[collection.sentinel-2-l2a.rule]]        # alpine scenes, mostly clear, for 48 h
bbox   = { intersects = [5.9, 45.8, 10.5, 47.8] }
cloud  = { max = 30 }
sensed = { within = "48h" }

[[collection.sentinel-2-l2a.rule]]        # spare capacity for under-replicated scenes
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
  follows an item change stream with a cursor (`GET /v1/collections/<ds>/items?since=`) and
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
  datasets. Only they count toward policy readiness. `replicas: 0` is allowed only for
  collections and means no guaranteed copies.
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
  default. The protected-version rule and surplus handoff apply only to blobs placed by
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
pins the proof chain and the complete catalog plus the selected items; `--path` and
`--max-size` still apply. Rescue rules need an index and are therefore cluster-only. Followers
report holdings to no cluster; opt-in public holdings reports are later work.

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

## Details still to specify

- Facet encodings (time precision, bbox normalisation), predicate semantics and vectors.
- `drop` record encoding, union-head history (which entries a writer appends for merged
  versions) and conflict resolution commands.
- Item API upload framing, batch format, push-log retention, credential scoping and
  per-producer rate limits.
- Holdings log encoding, limits, snapshot paging and index storage at 10M items × tens of
  members.
- Rescue convergence and oscillation damping when several rescue members act on the same
  stale counts.
- Catalog growth and expiry defaults. Completeness marks and atomic switches for a held
  view that is complete for the catalog plus selected items (whole or partial), shared
  with `--select` follows and D19 scopes.

## References

- [Decentralized Sentinel Collaborative Ground Segment](https://hackmd.io/@vmx/decentralized-collgs-202609)
  and [implementation notes](https://hackmd.io/@vmx/decentralized-collgs-implementation-202609)
- [collab-cluster-experiment](https://github.com/vmx/collab-cluster-experiment) and
  [collab-cluster-utils](https://github.com/vmx/collab-cluster-utils) (data-manager, rescuer)
- [Matadisco](https://matadisco.org/), [STAC](https://stacspec.org/),
  [STAC file extension (`file:checksum`)](https://github.com/stac-extensions/file)
- [JSON Pointer, RFC 6901](https://www.rfc-editor.org/rfc/rfc6901)
