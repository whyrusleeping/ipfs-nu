# Local API, client and CLI conventions

[Spec overview](../spec.md) · [Operational contracts](operations.md)

Normative detail for [spec.md "Local API, gateway and browsers"](../spec.md#local-api-gateway-and-browsers).
The Go types live in `api/apiv0`; the server in `api`, the client in `client`, the
daemon assembly in `node/daemon`. The served OpenAPI document is `GET /api/v0/openapi.json`
and must match the route table in `api.Routes` (a test enforces it).

## `$NU_PATH` layout

`$NU_PATH` defaults to `~/.nu`; any command initializes it on first use (`nu init` is
optional). Files are 0600, directories 0700.

| Entry | Contents |
|---|---|
| `config.toml` | Configuration; dotted keys, edited by `nu config get\|set <key> [value]`. `set` parses the value as TOML (`30`, `true`, `["a"]`, `"quoted"`) and falls back to a string. A running daemon applies changes after restart. |
| `node.key` | libp2p Ed25519 identity (0600), created by `nu init` or the first in-process node (`node.LoadKey`); its PeerId is the `peer=` hint of every share link |
| `nu.db`, `blobs/`, `partial/`, `staging/` | The store ([D17](data.md#d17-store-and-replication)): SQLite (DRISL blocks, inline blobs, pins, import cache; schema version in `PRAGMA user_version`), complete blobs with Bao outboards, partially fetched blobs, in-progress writes |
| `api`, `gateway` | URL of the running daemon's listeners, written at start and removed at clean shutdown; clients discover the daemon through `api` |
| `daemon.lock` | flock held by the running daemon, containing its PID; a second daemon fails with `conflict` |
| `tokens/<name>.json` | push tokens of the producer listener (0600; `nu api token`; deleting the file revokes it) |
| `public.cert`/`public.key` (config) | the public listener's PEM chain and key live wherever the operator keeps them, outside `$NU_PATH` |
| `cluster/net/ingest/` | retained bytes of interrupted item URL downloads, with their validators |
| `version` | layout version (`1`); a newer one is refused untouched ([U4](operations.md#u-upgrades-and-compatibility)) |

Back up keys and signed state with `nu repo backup` and content with `nu repo export`
([B1–B8](operations.md#b-backup-export-and-recovery)); copying the directory of a running
daemon is not a consistent backup. Linux servers use `NU_PATH=/var/lib/nu`
([I3](operations.md#i-distribution-and-service-lifecycle)).

Default config: `api.listen = "127.0.0.1:5171"`, `gateway.listen = "127.0.0.1:8171"`,
`swarm.listen = ["/ip4/0.0.0.0/udp/4171/quic-v1", "/ip6/::/udp/4171/quic-v1"]`,
`store.limit = "0"` (physical budget, unlimited), `store.free_reserve = "1G"`,
`store.gc_watermark = 0.9` ([D17 A5](data.md#accounting-and-budgets); sizes take a
K/M/G/T/P suffix or a bare byte count). The daemon refuses non-loopback API/gateway
listeners until authentication exists. `[public]` (off by default) configures the
[public listener](#public-listener-0043).

## Error envelope and exit categories

Every error response is `{"error": {"code", "message", "action"?, "details"?}}`. Codes
are stable; messages and actions are plain language and may change. The CLI prints
`nu: <message>` and `  → <action>` (or the envelope on stderr with `--json`) and exits
with the code's category. Flag and argument mistakes exit 2 like Go's `flag` package.

| Code | HTTP | Exit | Meaning |
|---|---|---|---|
| `failed` | 500 | 1 | unclassified failure, failed operation |
| `invalid_argument` | 400 | 2 | malformed target, flag, cursor or body |
| `not_found` | 404 | 3 | unknown target, operation, route or config key |
| `not_supported` | 422 | 4 | valid input this version cannot act on yet (names, datasets) |
| `conflict` | 409 | 5 | daemon already running, operation already finished |
| `timeout` | 408 | 6 | a wait expired; `details.operation` holds the current snapshot |
| `unavailable` | 503 | 7 | no daemon reachable, or shutting down |
| `forbidden` | 403 | 8 | loopback/origin protections or authentication |
| `cancelled` | 409 | 9 | operation cancelled, command interrupted |
| `not_implemented` | 501 | 10 | endpoint exists but is not in this build |
| `insufficient_storage` | 507 | 11 | a store budget refused the write ([D17 A6](data.md#accounting-and-budgets)); `details` carry `{reason: store_limit\|free_space, need, headroom, physical, limit, free, reserve}` |

## Paging

List endpoints take `?cursor=` and `?limit=` (default 100, max 1000) and answer
`{"items": [...], "next_cursor"?, "resync"?}`. Cursors are opaque (base64url of
`<epoch>:<offset>`); a cursor whose epoch no longer matches (daemon restart, store
generation change) yields an empty page with `"resync": true`, and the client lists again
from the start instead of treating it as an empty delta. A malformed cursor is
`invalid_argument`.

## Operations

Long-running work is an operation: `{id, kind, target, state, stage?, progress{done,
total?, unit?}, created, updated, result?, error?}`. `target` is exact (a CID, path or
link), never something that could resolve differently later. States: `running`,
`succeeded`, `failed`, `cancelled`. Operations live in the daemon's memory (not across
restarts); finished ones are kept for up to an hour, at most 1000.

Events (`GET /operations/{id}/events?since=<seq>`, `application/x-ndjson`): one
`{seq, time, type, stage?, progress?, message?, operation?}` per line; `seq` starts at 1
and is the resume cursor. Types: `stage`, `progress`, `log`, `done`. The `done` event
carries the final operation and is always the last line. Progress reporters rate-limit
themselves (a few events per second).

`GET /operations/{id}/wait?timeout=30s` (max 5m per request; the client loops for longer
waits) returns the operation once finished, else a `timeout` error with the snapshot.
`POST /operations/{id}/cancel` cancels a running one. The CLI follows an operation with
`nu operations wait <id>` or by default after `nu add`/`nu pin add` (`--no-wait` prints
the operation instead); with `--json` it prints the NDJSON events.

## Routes (v0)

```text
GET    /api/v0/status                      daemon summary (version, pid, paths, operation counts)
GET    /api/v0/openapi.json                this API's description
GET    /api/v0/operations                  list (paged)
GET    /api/v0/operations/{id}
GET    /api/v0/operations/{id}/events      NDJSON stream
GET    /api/v0/operations/{id}/wait
POST   /api/v0/operations/{id}/cancel
POST   /api/v0/add        {path, recursive?, raw?, hash?, nocopy?, no_announce?, pin?} → 202 operation (result: AddResult)
GET    /api/v0/cat?target=                 bytes
GET    /api/v0/ls?target=                  listing
GET    /api/v0/pins                        list (paged)
POST   /api/v0/pins       {cid, name?}     → 202 operation (result: Pin)
DELETE /api/v0/pins/{cid}                  → 204
GET    /api/v0/dag?target=                 atproto-style JSON
POST   /api/v0/dag        JSON body        → {cid}
POST   /api/v0/get        {target, output?, dir?} → 202 operation (result: GetResult)
POST   /api/v0/share      {target}         → ShareLink
GET    /api/v0/cluster                     ClusterStatus {local?, network?}
POST   /api/v0/cluster/init     {name, mode?}                    → ClusterInfo
POST   /api/v0/cluster/invites  {role?, tags?, capacity?, peer?, expires?} → ClusterInvite
POST   /api/v0/cluster/join     {token}                          → ClusterInfo
DELETE /api/v0/cluster/members/{peer}      → MetadataState
GET    /api/v0/cluster/datasets            [Dataset]
POST   /api/v0/cluster/datasets {name, replicas?, writers?, note?, collection?, facets?, merge?, expire?} → Dataset
GET    /api/v0/cluster/datasets/{name}     Dataset
GET|POST /api/v0/datasets, GET /api/v0/datasets/{name}   the same, on the local cluster
GET    /api/v0/cluster/collections/{name}                CollectionStatus
GET    /api/v0/cluster/collections/{name}/items          ?since=|cursor=&limit=&prefix=&holder=&held=&unheld=&rarest=&match= → Page[CollectionItem]
GET    /api/v0/cluster/collections/{name}/items/{path…}  CollectionItem (holders, copies, conflicts)
POST   /api/v0/cluster/collections/{name}/hold|release   {path} → CollectionItem
GET    /api/v0/cluster/collections/{name}/rules          CollectionRules (with current impact)
PUT    /api/v0/cluster/collections/{name}/rules?dry_run= {rules: [rule…]} → CollectionRules
GET    /api/v0/cluster/collections/{name}/conflicts      [CollectionConflict]
GET    /api/v0/cluster/metrics                           Prometheus text: collection aggregates
POST   /api/v0/cluster/collections/{name}/items?wait=written&timeout=  CollectionPushRequest → 202|200 CollectionPush
POST   /api/v0/cluster/collections/{name}/items:batch    NDJSON CollectionPushRequest lines → NDJSON CollectionPushBatchLine
DELETE /api/v0/cluster/collections/{name}/items/{path…}?wait=written → CollectionPush (state removed)
GET    /api/v0/cluster/collections/{name}/pushes         ?since=<cursor>|0&limit= → Page[CollectionPush]
POST   /api/v0/cluster/collections/{name}/uploads        application/octet-stream body → CollectionUpload {cid, size}
GET    /api/v0/at/record?target=at://…      AtRecord (verified record JSON with commit and rev)
POST   /api/v0/at/snapshot  {target, blobs?} → 202 operation (result: AtSnapshot)
POST   /api/v0/car/import   {path, profile?, key?, offline?, pin?} → 202 operation (result: CarImport)
GET    /api/v0/car?target=<cid>&profile=    CAR bytes (application/vnd.ipld.car)
GET    /api/v0/public                       Page[PublicRoot] (0043)
POST   /api/v0/public       {cid}           → PublicRoot (https/site/rasl set while the public listener runs)
DELETE /api/v0/public/{cid}                 → 204
POST   /api/v0/auth/login   {identifier}    → 202 LoginStart {operation, url, did, handle, pds}; the operation's result is Account (0045)
GET    /api/v0/auth/accounts                [Account]
DELETE /api/v0/auth/accounts/{account}      → 204 (`-`: the only account)
POST   /api/v0/names        PublishRequest  → PublishResult {publication, wait?, note}; a timed-out seed wait is 408 with details.result
GET    /api/v0/names?account=               NameList (each name verified)
DELETE /api/v0/names/{name}?account=        → Publication (the deletion's rev)
GET    /api/v0/resolve?target=&max_age=&fresh=1&offline=1   NameResolution {target, rev, source: pds|cache, age, held}
POST   /api/v0/node/register {label?, seed?} → NodeRecord
GET    /api/v0/node/devices?account=        DeviceList
POST   /api/v0/follows      FollowRequest   → 201 Follow (mirror dataset with a name source)
GET    /api/v0/follows                      [Follow] {observed, syncing, held, state, blockers, content}
DELETE /api/v0/follows/{dataset}            → Follow (state unfollowed, orphans = released pins)
POST   /api/v0/follows:refresh{dataset?}   → [Follow]
```

The gateway also serves `/nu/<handle-or-did>/<name>[/path]` (0045): a 302 to the immutable
`/tree/<cid>/…` of the verified target (`X-Nu-Root`, `X-Nu-Source: pds|cache`); `?held=1`
selects a local follow's held snapshot (`X-Nu-Source: held`), which may be older.

`add` also takes `{url, meta?}` (URL import) and `meta` for local paths; the result then
carries `source` (provenance), `meta` and `entry`.

`add` and `get` pass absolute local paths: API and daemon run as the same user on
loopback, so the daemon reads and writes the files itself. GET never changes state;
`share` is a POST because it will also announce the content once a host runs.

## Content commands (v0)

`node.Local` implements the content methods on the store (`store/`), with resolution in
`node/content`; the daemon serves the same object over the API.

- **`add <file>`** streams the file into a blob (BLAKE3 by default; `--hash sha256` for
  a core-DASL blob of at most 8 MiB, refused up front with `invalid_argument` otherwise)
  and returns a single-mode MASL file node `{src, "io.ipfs.nu": {size, name, mode?}}`
  ([D3a](data.md#d3a-single-file-nodes)); no `content-type` is recorded (type guessing is
  machine-dependent; the gateway falls back to the name's extension). `--raw` returns the
  blob CID instead. **`add -r <dir>`** builds the tree with `tree.BuildFS` and returns the
  `io.ipfs.nu.tree` header CID; unsupported entries (devices, sockets, FIFOs) are listed
  in `AddResult.skipped` and logged as operation events. `--nocopy` references files in
  place (`store.AddNoCopy`, BLAKE3 only). The result is pinned (`explicit`, `recursive`
  under the `nu-tree` profile; `direct` for a raw blob) unless `pin` is false.
  **Import cache:** the store records `(path, hash) → (size, mtime, inode, cid)` for every
  file it adds; an unchanged file whose blob is still held is not re-hashed on re-add (a
  `--nocopy` reference is only reused by another `--nocopy` add). The operation reports
  stages `scanning` (directories), `adding`, `pinning` and byte progress; a cancelled
  operation stops at the next read.
- **`cat <cid[/path]>`** streams a file node's blob, a raw blob, or a file inside a tree;
  a directory is `invalid_argument`. **`ls`** lists a directory (tree header, a directory
  MST node, or a path to one) as `{name, cid, type: file|dir|symlink, size}` in name
  order; for a file node or blob it returns that one entry. Content that is not held is
  `not_found` with an action.
- **`pin add <cid>`** is an operation (stage `checking` walks the content, `pinning`
  records it); content that is not fully held fails with `not_found` until fetching
  arrives. **`pin rm`** removes every explicit pin of the CID; **`pin ls`** lists
  `{cid, name?, mode, source}` for all sources. **`public add|rm|ls`** manage the
  `public` source for the [public listener](#public-listener-0043) (`add` requires held,
  complete content and is synchronous).
- **`dag get <cid[/path]>`** prints a DRISL object as atproto-style JSON (`$link`,
  `$bytes`); a path selects map keys and array indexes and follows links on the way, and a
  final link is printed as `{"$link"}`. A raw CID is `invalid_argument` (use `cat`).
  **`dag put`** parses atproto-style JSON (strict DRISL; foreign links refused), stores
  the SHA-256 DRISL object and prints its CID; it is not pinned.
- **`get <cid-or-link> [-o path]`** is an operation (stages `resolving`, `fetching` when
  something is not held, `writing` with byte progress) whose result is
  `GetResult{cid, path?, output, kind, files, dirs, bytes, skipped?, fetched?}`. The
  target is a CID with optional path or a `nu://` link (kept exact as the operation
  target; the link's `peer=`/`addr=` hints go to the fetcher). `output` is the exact
  destination; without it the content is written into `dir` (the CLI's working directory)
  under the path's last segment, the file node's name, or the CID. A tree becomes a
  directory, a file node or blob a file, written with [safe extraction](#safe-extraction-nu-get--o);
  symlink entries are skipped and reported; a symlink target itself is
  `invalid_argument`. **Fetch seam:** when the root (or structure on the way to the path)
  is missing, or the walk under the resolved node is incomplete, the node calls
  `node.Fetcher.Fetch(ctx, cid, hints, progress)` (the whole root when the root is
  missing, the resolved sub-node otherwise) and resolves again. `node.Local.Fetcher` is
  `node.NoFetcher` until the multi-peer fetcher (0027) is plugged in, so a missing target
  fails with `not_implemented` in this build.
- **`share <cid[/path]>`** returns `ShareLink{target, link, peer_id, addrs, serving}` for
  content that is complete in the store (`not_found` otherwise, with the missing CID in
  `details.missing`). The link names this node (`node.Local.PeerID()`) with
  `node.Local.Addrs()` (see [Links](names.md#links)); `serving` is false until the daemon
  runs a host (`node.Local.Net`, the seam a host fills with its addresses and peer count).
  The CLI prints the link; with a daemon it exits, without one `serveForeground` is where
  the in-process node will keep serving until interrupted — in this build it explains
  that content is available only while a holder is online and returns. `add` fills
  `AddResult.link` the same way.
- **`status`** carries a `store` section (`store.Stats`, [D17 A8](data.md#accounting-and-budgets))
  `{objects, pins, bytes, blobs, outboards, partials, db, staging, reserved, nocopy, free,
  limit, reserve, headroom, pressure}`: physical bytes by class (each object charged
  once; `--nocopy` sources reported, not charged), the budgets and the headroom left
  (`-1` unknown/unlimited), and a `network` section `{peer_id, addrs, serving, peers}`
  from the identity and address seams. The CLI reads them from the daemon or, when none
  runs and the repo lock is free, from an in-process node, and prints a `budget:` line
  with the pressure state and its action.
- The store admits every write against the budgets (A6); a refusal is
  `insufficient_storage` from the operation (`add`, `get`, fetches, `dag put`) with the
  shortfall in `details`. The daemon runs `store.GCIfNeeded` every `node.MaintainInterval`
  (10 min) and the store runs it on a shortfall; GC passes `node.GCGrace` so unpinned
  objects of in-progress adds and fetches survive, and never removes pinned content.

## atproto records, snapshots and CARs (0053)

`node.AtprotoNode` (implemented by `node.Local` and `client.Client`) carries the at://
and CAR commands; `ls`/`cat` accept at:// targets through the ordinary routes. The
`atproto` package does every network step through `httpsrc` (SSRF guards, no ambient
proxy or cookies) and verifies with `core/atrepo` before anything is returned or kept
([D6](names.md#d6-resolution-and-caching), [D21](dasl.md#using-atproto-data)).

- **Identity.** A handle resolves by DNS TXT `_atproto.<handle>` (`did=…`), then
  `https://<handle>/.well-known/atproto-did`; the DID document (did:plc from
  `plc.directory`, did:web from `/.well-known/did.json` or the path form) supplies the
  `#atproto` signing key and the `#atproto_pds` endpoint, which must be an https origin
  that passes the SSRF policy. A handle must appear in the document's `alsoKnownAs`
  (bidirectional check). Identities are cached for 10 minutes; `AtRecord.identity_age`
  exposes the age. A signature failure re-resolves once (key rotation).
- **`get at://<did|handle>/<collection>/<rkey>`** (`GET /at/record`) fetches
  `com.atproto.sync.getRecord`, verifies the proof (commit signature with the document
  key and DID, MST path, record block) and returns
  `AtRecord{uri, did, handle?, pds, collection, rkey, cid, commit, rev, value, source:
  "pds", identity_age, data_model?}`; the CLI prints it as JSON (or writes it with `-o`).
  The proof blocks are stored unpinned, so `dag get <commit|record cid>` works on them
  until GC; nothing is pinned per record. A proven-absent record is `not_found`.
- **`get at://<did|handle> [--blobs]`** (`POST /at/snapshot`) is an operation (stages
  `resolving`, `fetching` with byte progress, `blobs`, `pinning`) that streams
  `com.atproto.sync.getRepo` into the store (every block verified by CID; budget refusals
  are `insufficient_storage`), verifies the whole repository (signature, every MST node
  rebuilt and compared with `data`, every record), and pins the commit recursively under
  the `atrepo` profile with pin name `at://<did>@<rev>`. A verification failure pins
  nothing (the blocks fall to GC) and names what failed. With `--blobs` the records'
  blob refs (`{$type: "blob"}` and legacy refs, from the `atrepo+blobs` walk) are fetched
  with `getBlob`, verified as SHA-256 raw CIDs and the pin uses `atrepo+blobs`; blobs over
  8 MiB are skipped and counted in `blobs_oversized`, failures in `blobs_failed`, at most
  `Limits.MaxBlobs` (10,000) per snapshot. Each `get` makes a new snapshot pin; older
  revs stay pinned until `pin rm <commit>`.
- **Browsing.** `ls at://<did|handle>[@<rev>]` lists the held snapshot's collections
  (`{name, cid: commit, type: "collection", size: record count}`), `ls …/<collection>`
  its records (`type: "record"`, size in bytes), `ls …/<collection>/<rkey>` one record;
  `cat at://…/<collection>/<rkey>` prints the record as JSON. Without `@rev` the newest
  held rev is used; a handle is resolved to its DID first (network). When no snapshot is
  held, `ls` snapshots the repository first (like `ls <cid>` fetches) and `cat` does a
  live verified record fetch. `get`, `dag get` and `share` with an at:// target explain
  the at:// commands (`invalid_argument`).
- **`car import <file> [--profile nu-tree|atrepo|drisl] [--key k] [--offline] [--no-pin]`**
  (`POST /car/import`, operation with stages `reading`, `verifying`, `pinning`) reads a
  DASL CAR with exactly one root from an absolute path (the daemon reads the file), stores
  every block (verified by CID, at most 2²⁴), and verifies under the profile, detected
  from the root block when omitted (v3 commit → atrepo, tree header or file node →
  nu-tree, else drisl). `atrepo` verifies the structure completely and the signature
  with `--key` (did:key or `publicKeyMultibase`) or, otherwise, with the key of the
  commit DID's document resolved online; `--offline` (or a failed resolution) reports
  `signature: "unverified: …"` and still imports. `nu-tree` requires a tree header or
  file node root; `nu-tree` and `drisl` require every reachable block to be present.
  The result is `CarImport{root, profile, blocks, bytes, unreferenced?, objects,
  pinned, did?, rev?, records?, non_conforming?, signature?, signed_by?}`; `atrepo`
  imports are pinned as snapshots (`at://<did>@<rev>`), others as explicit recursive pins.
- **`car export <cid> [--profile p] [-o file]`** (`GET /car`) streams a held, complete
  root: `atrepo` in streamable order (`atrepo.Export`: commit, MST pre-order with records
  interleaved, each block once, byte-identical blocks so a PDS CAR re-exports as a
  reordered file that stock tools verify), `nu-tree` and `drisl` in pre-order
  (`car.PreOrder`). Raw blobs over 8 MiB cannot be CAR blocks and fail the export.
  Incomplete content is `not_found` with the missing count.
- **Gateway** `/at/<did>[@<rev>]/`, `/at/<did>[@<rev>]/<collection>/` and
  `/at/<did>[@<rev>]/<collection>/<rkey>` serve held snapshots only (never a live
  repository, P4): listings are gateway pages with the listing CSP whose rows link only
  syntactically valid NSIDs and record keys (others are shown unlinked), a record is
  `application/json` with the listing CSP, `nosniff` and `ETag: "<record cid>"`; blob
  refs stay CIDs for `/blob/<cid>` (always a download). The latest view is `no-cache`,
  a fixed `@<rev>` immutable. `/at/` has no site origin: on `<cid>.localhost` the path is
  a tree path (rules G5/G9, matrix row N30).
- **Limits** (`atproto.Limits`): DID document 64 KiB, well-known handle body 1 KiB,
  proof CAR 8 MiB, repo CAR 4 GiB (30 min), blob 8 MiB, 4 concurrent requests per PDS
  host, 30 s per small request. Loopback, private and special-use addresses are refused
  for every endpoint, including PDS URLs taken from DID documents.

## Clusters (0062, 0046)

`node.ClusterNode` (implemented by `node.Local` and `client.Client`) carries the cluster
commands; state lives under `$NU_PATH/cluster/{local,net}` ([D20](cluster.md#state-encoding-and-evaluation)).
`nu dataset create|ls|status` act on the implicit local cluster, created on first use;
`nu cluster init|invite|join|status|member rm|dataset …` on the at most one networked
cluster. `init`, `invite`, `member rm` and dataset writes work daemonless (in-process,
offline); `join` needs networking and returns after the first state sync. Every write
reports `metadata` (`MetadataState`: signer, commit, rev, `acked_by` storage members,
`required`, `acknowledged`) apart from `content` (`ContentState`); metadata
acknowledgment never implies stored content (E10).

- **Content status (0046):** `ContentState.state` is `none` without a head, else one of
  the [E-rule](operations.md#e-readiness-evidence) values `ready`, `syncing`, `unknown`,
  `degraded`, `policy_unsatisfiable`, `paused`, `error`; clients must treat any state
  other than `ready` as not ready. It carries the [W3](operations.md#w-exact-target-waits)
  fields `target`, `mode`, `epoch`, `policy`, `confirmed: [{peer, seq, age_ms}]`,
  `required` (confirmed copies per unit), `copies: {unit, units, below, min}`,
  `missing: [{peer, reason}]`, `extra`, `evidence_age_ms`, `unmet`, plus
  `last_ready` (E8), `local` (this node's own sync: target, observed head when staging is
  full, mode, state, owned/missing counts, pause reason and bytes needed) and `release`
  (`auto: false`; last ready, last held, staged and candidate versions, all still pinned).
  Collections report `policy_unsatisfiable` with `replicas: 0` (no guaranteed copies);
  with floor placement the target is the merged catalog root and the unit `item`.
  `CollectionItem.owners` (single-item query) lists an item's floor owners and
  `CollectionStatus.floor {eligible, epoch, owned, complete, bytes, surplus}` this node's
  floor holdings.
- **Head writes:** `POST /datasets/{name}/head` and `/cluster/datasets/{name}/head`
  (`DatasetUpdateRequest {target, kind?, note?}`, `nu [cluster] dataset update <ds> <cid>`)
  write the head record; `kind` is detected from held content. The `Dataset` result has
  `metadata` and `content` as separate results.
- **Waits:** `GET /datasets/{name}/wait?target=&condition=metadata|held|ready&holder=&acks=&mode=&timeout=`
  (and `/cluster/datasets/{name}/wait`; `nu [cluster] dataset wait <ds> <cid> [--condition]
  [--holder]… [--acks] [--timeout]`) answers a satisfied `WaitResult` (the `ContentState`
  plus `condition`, `satisfied`, `superseded_by`, `metadata`). An unsatisfied wait is the
  error `timeout` (exit 6) or `failed` (policy unsatisfiable, holder not a member), with
  the result in `details.result`. A target no head names yet is unmet
  (`target_not_observed`), not a failure. `archive-ready` (datasets with history) is E6
  over the live set current when the wait starts; the result carries `live`.
- **Policy:** `DatasetCreateRequest` takes `replicas` or `all`, `where` (tag → values) and
  `spread` (`--replicas k|all --where zone=eu-1|eu-2 --spread zone`). A networked dataset
  with `replicas` > 1 and no `spread` gets `spread: zone` only when every eligible member
  is zone-tagged and there are enough zones; otherwise the result carries a warning.

Writing without a suitable key is `forbidden`; a second cluster
or a dataset name already used in either cluster is `conflict`. In private and isolated
mode the daemon disables mDNS and gates its host to members from start-up.

## Version history (0047)

`node.HistoryNode` (`node.Local`, `client.Client`) carries archives and versions
([cluster.md "Version history"](cluster.md#version-history)); both scopes, local routes
shown (`/cluster/datasets/…` for the networked cluster):

```text
GET  /datasets/{name}/versions             DatasetVersions {history, live, live_bytes, excess_bytes?, archive, versions}
GET  /datasets/{name}/diff?from=&to=       DatasetDiff {added, removed, changed, truncated?}
POST /datasets/{name}/label    VersionLabelRequest {version, label?, note?, remove?, reacquire?} → VersionLabel
POST /datasets/{name}/rollback RollbackRequest {version, note?} → Dataset (version: the appended one)
POST /datasets/{name}/prune    PruneRequest {dry_run?} → PruneReport {candidates, free_bytes, shared_bytes, pruned?}
nu [cluster] dataset create <ds> --archive|--mirror [--history keep_last=10,daily=30,…] [--source <dir>]
nu [cluster] dataset update <ds> [<cid> | <dir>]     # no target: re-add the --source directory
nu [cluster] dataset versions|diff|label|rollback|prune <ds> …
```

- `DatasetCreateRequest` takes `archive`, `mirror`, `history` (`HistoryPolicy`, counts
  with `-1` = all) and `source_path`; `Dataset` reports `history`, `source_path`,
  `archive` (archive-wide `ContentState` with `live`) and, after a head write or
  rollback, `version`. A head write on a dataset with history writes the version record
  in the same commit. The CLI imports a directory with an unpinned `add -r` and then
  writes the head (the dataset's pins keep it); API clients do the same two steps.
- A version selector is a TID, a label or a time (RFC 3339, or `YYYY-MM-DD` = end of
  that day UTC; the newest version at or before it). Unknown versions are `not_found`, a
  label on two versions `conflict`. `VersionInfo.availability` is one of
  `retained-complete`, `fetching`, `observed`, `degraded`, `unknown`, `skipped`,
  `pruned`; reads (`<ds>@<v>` in `get`/`cat`/`ls`, gateway `/c/`), rollback and labels of
  a version that is not `retained-complete` are `unavailable` with `details.version` and
  `details.availability` (labels pass with `reacquire`). A rollback to the current head
  is `conflict`; a dataset without history is `invalid_argument`.
- `prune` releases only on the local cluster; on a networked cluster it is
  `not_supported` unless `dry_run` (0048). Released bytes go at the next store GC.

## Collections (0057)

`node.CollectionNode` (`node.Local`, `client.Client`) carries the collection queries of
[D22](collections.md); the types in `api/apiv0/collection.go` are the preview contract that
the item push API (0059) and dashboards build on. State lives in
`$NU_PATH/cluster/net/collections.db`. `nu cluster dataset create <ds> --collection
--facets facets.toml [--expire sensed=400d] [--merge lww]` creates one (`replicas`
defaults to 0). `nu cluster collection status|items|where|rules|hold|release|conflicts
<ds> …` and `nu cluster where <ds>/<item>` query it; `items` takes `--since <cursor>|0`
(the item change stream), `--rarest`, `--holder <peer>`, `--unheld`, `--held`,
`--prefix` and `--facet name.op=value` (ops `eq`, `in` (comma list), `min`, `max`,
`within`, `intersects` (w,s,e,n)); `rules --set rules.toml|rules.json [--dry-run]`
replaces this node's rules (TOML `[[rule]]` tables or a JSON list of flat rule objects).

- **Item stream:** `items?since=0` starts at the beginning; every page carries
  `next_cursor` (the stream position, also when empty), so a consumer polls with it. Each
  changed path appears once with its latest state (`removed: true` when it left the
  catalog); a cursor from another database epoch answers `resync: true`. Without `since`,
  `items` lists current items in path order (or rarest first) with ordinary paging.
- **Copies** (`copies`, `unknown`) and `rarest`/`unheld` need the holdings index on the
  answering node (`cluster.index = true`, or any rescue rule); without it `rarest` and
  `unheld` are `unavailable` and `copies` is omitted.
- **Metrics:** `nu_collection_items`, `_bytes`, `_conflicts`, `_local_items{state}`,
  `_local_bytes`, `_shortfall_bytes`, `_holdings_log_seq`, and with the index
  `_unheld_items`, `_copies_items{copies}`, `_member_items{peer}`, all labelled
  `collection`.

## Item pushes (0059)

`node.PushNode` (`node.Local`, `client.Client`) carries the push path of
[D22 "Ingest"](collections.md#ingest); its semantics are in
[collections.md](collections.md#item-pushes-and-url-import-preview-0059).

```text
nu cluster collection put <ds> <path> --meta meta.json [[name=]file|dir]… [--url name=url]…
    [--checksum name=<hex multihash>]… [--nocopy] [--upload] [--replace] [--wait]
nu cluster collection put <ds> --batch items.ndjson
nu cluster collection rm <ds> <path> [--wait]
nu cluster collection pushes <ds> [--since <cursor>|0]
nu cluster dataset create <ds> --collection [--ingest-hosts h,…] [--ingest-attempts n]
    [--ingest-timeout 2h] [--ingest-max-size 1T]
nu add <https-url> [--hash sha256] [--meta meta.json]
```

- `CollectionPushRequest` is `{path, meta: {…}|"<cid>", files: {<name>: {upload} |
  {path, nocopy?} | {url, checksum?, size?}}, replace?}`; `CollectionPush` is `{path, id,
  state: accepted|fetching|written|failed|removed, duplicate?, replace?, entry?, size?,
  meta?, head?, sources?: {<name>: URLSource}, tries?, error?, seq, created, updated}`;
  `URLSource` is `{url, final_url?, etag?, last_modified?, content_type?, size, checksum?,
  resumed?}`. The single-item query (`GET …/items/{path…}`) adds `push` on the receiving
  member, and answers `{path, push}` for an item pushed there but not yet in the merged
  catalog.
- `put` sends local files as paths (the daemon reads them) on a loopback API, and uploads
  them first with `--upload` or a remote `--api`; a directory adds every regular file
  below it under `name/`. `--wait` waits up to 5 min. A failed push exits with its error's
  category.
- Item URLs and `nu add https://…` use `httpsrc` with trusted node config
  `ingest.allow_http` (default false) and `ingest.allow_ips` (address prefixes exempt from
  the SSRF guard). `nu add` imports a URL as a BLAKE3 file node named after the URL's last
  path segment (`download` otherwise), with up to 4 attempts that resume on a strong
  validator; `--hash sha256` keeps the 8 MiB limit; isolated-cluster nodes refuse it.
  `--meta` stores a metadata record (adding the provenance under `source` unless present),
  pins it and returns the tree `entry` that carries it.
- Body types: `items:batch` takes `application/x-ndjson`, `uploads` takes
  `application/octet-stream`; neither is CORS-safelisted, so A5's rationale holds
  ([A5](operations.md#a-local-api)).

### Producer listener and push tokens

Producers on other machines push through a separate listener (rule
[A8](operations.md#a-local-api)), off by default:

```toml
[api.producer]
listen = "0.0.0.0:5172"
hosts = ["nu1.lan", "10.0.0.5"]   # Host header allowlist (A2); "name:port" to pin a port
trusted_network = true            # required: this build has no TLS
```

It serves only `POST …/items`, `…/items:batch`, `…/uploads`, `DELETE …/items/{path…}`,
`GET …/items/{path…}` and `GET …/pushes`, each with `Authorization: Bearer <token>` for
a token whose collections include `{name}` (`forbidden` otherwise); A3–A7 apply as on the
loopback API. `nu api token create <name> --collection <ds>…` creates a token
(`nu_pt_` + 32 random bytes, base64url) in `$NU_PATH/tokens/<name>.json` (0600) and prints
it; `ls`, `rotate <name>` and `rm <name>` manage them; the daemon reads the directory on
every request, so rotation and revocation apply immediately. Producers run
`nu --api <host>:<port> --token <token> cluster collection put …` (or set
`NU_API_TOKEN`). The loopback API stays unauthenticated (A1–A7), and `api.listen` is still
refused off loopback.

## Loopback protections

Mechanics of rules A1–A7 of the [security contract](operations.md#a-local-api). The API
accepts only requests whose `Host` is a loopback name or address (`localhost`,
`127.0.0.1`, `[::1]`) with this listener's port (DNS-rebinding defense). Requests other
than GET/HEAD are rejected with `forbidden` when they carry an `Origin` that is not the
API's own `http://<host>` or a `Sec-Fetch-Site` other than `same-origin`/`none`; the
gateway's origin is therefore rejected too. `OPTIONS` is refused and no `Access-Control-*`
header is ever sent, so a browser page cannot read responses or change state. Request
bodies are read only as `Content-Type: application/json` (`invalid_argument` otherwise),
which HTML forms cannot send and scripts cannot send without the refused preflight.
Responses carry `X-Content-Type-Options: nosniff` and `Cache-Control: no-store`; cookies
are never set or read. The daemon refuses non-loopback listeners (A1) until A8 exists.

## Gateway (v0 baseline)

The loopback gateway (`gateway.listen`, default `127.0.0.1:8171`) serves held content
through the `gateway.Content` interface (`Resolve(root, path)`, `Open`, `List`), which
`node/content` implements on the store: tree headers and directory nodes list and resolve
paths, file nodes and tree entries supply name, MIME type and mode, raw blobs are bytes.
These are the mechanics of rules G1–G9 of the
[security contract](operations.md#g-gateway-and-browsers); G5 (name origins) and G10
(MASL headers) are not implemented yet.

- **Hosts.** `Host` must be `localhost`, a loopback IP literal, or `<cid>.localhost`
  (one label, a syntactically valid base32 CIDv1), always with the gateway's own port.
  Anything else is `403` (DNS-rebinding defense).
- **Two origins.** The bare origin (`http://localhost:8171`, `http://127.0.0.1:8171`)
  serves `/tree/<cid>/<path>` and `/blob/<cid>` as *non-active* content only. One origin
  per root, `http://<cid>.localhost:8171/<path>`, serves the tree as a site: `index.html`
  when a directory has one, listings otherwise, active content allowed because the origin
  isolates it. Browsers that do not resolve `*.localhost` (Safari) keep the bare origin's
  download behavior; nothing active is ever rendered there.
- **Non-active fallback.** On the bare origin a file is served inline only if its
  sanitized type is `text/plain`, `text/css`, `application/json`,
  `application/octet-stream`, or `image/*` (not SVG), `video/*`, `audio/*`, `font/*`.
  Everything else (HTML, SVG, XML, JavaScript, PDF, unknown) is sent as
  `application/octet-stream` with `Content-Disposition: attachment` and a
  `Link: <http://<cid>.localhost:<port>/<path>>; rel="alternate"` header. Listings carry
  an "open as site" link to the same URL.
- **Types.** The tree's `mime` is parsed with `mime.ParseMediaType`; only a `utf-8`
  charset parameter survives; an invalid or missing type falls back to the name's
  extension, then `application/octet-stream`. Blobs (`/blob/<cid>` or a blob root) are
  always `application/octet-stream` downloads. Symlink entries are listed but answer
  `422` instead of being followed.
- **Headers.** Every response has `X-Content-Type-Options: nosniff`; content responses
  carry `ETag: "<cid>"` and `Cache-Control: public, max-age=31536000, immutable`;
  gateway-generated pages (root, listings) carry
  `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`. No cookies are
  ever set or read. Only `GET`/`HEAD` are accepted; `Range`, `If-Range` and
  `If-None-Match` work on files. Errors are plain text (`<code>: <message>`), never HTML.
- **Paths.** Paths with empty (`//`), `.` or `..` segments or NUL are `400` (an empty
  segment is refused on every route before the mux could clean or redirect it, so
  `//host` can never become a scheme-relative `Location`); a directory URL without a trailing slash redirects (`301`) to its own
  escaped path plus `/`. Listings show the directory CID and total size below it, and each
  entry's name, kind, size and CID; symlinks and entries with unsafe names are shown but
  not linked. The MASL header allowlist (D21, G10) is not applied yet.
- **`/at/` routes** serve held atproto snapshots as inert JSON and listings; see
  [atproto records, snapshots and CARs](#atproto-records-snapshots-and-cars-0053).
- **Datasets (0047).** `GET /c/<dataset>[@<version>][/<path>]` answers `302` (with
  `Cache-Control: no-store` and `X-Nu-Root`) to `/tree/<root>/<path>`, so every read binds
  to one immutable root under the bare origin's non-active rules; an unavailable version
  is an error naming its availability, never another version. On `<cid>.localhost`, `/c/`
  is an ordinary tree path.
- **RASL (0052).** `GET`/`HEAD /.well-known/rasl/<cid>` serves one held object's bytes as
  stored (raw blob or DRISL node) through `gateway.ObjectContent` (`node/content`):
  `application/octet-stream`, `Content-Disposition: attachment`, `ETag`, immutable
  caching, `Range`; query string ignored; `404` for anything not held (nothing is
  fetched), `400` for a non-CID, no CORS header on loopback. See [D21](dasl.md#rasl).

## Public listener (0043)

The public HTTPS seed is the gateway handler in public mode on its own listener
(rules [P1–P6](operations.md#p-public-and-member-only-serving); runbook under P6).

```toml
[public]
listen = ":443"            # off when absent/empty
domain = "seed.example"    # sites on <cid>.seed.example; "seed.example:8443" when not 443
cert = "/etc/nu/fullchain.pem"
key = "/etc/nu/privkey.pem"
# behind_proxy = true      # instead of cert/key: plain HTTP for a TLS proxy; listen must be loopback
# max_requests = 256       # concurrent requests, total
# per_ip = 32              # concurrent requests per client address
```

- **Hosts.** `Host` must be exactly `domain` (a trailing dot and `:443` are ignored) or
  `<cid>.domain`; anything else is `403`. `domain` must be a DNS name with two or more
  labels: no IP literal, `localhost` or `*.localhost` (P5).
- **What is served.** Only CIDs under a *public root*: `nu public add <cid>` marks a
  held, complete tree, file or blob (the mark is a pin with source `public`, so GC keeps
  it); `nu public rm` unmarks; `nu public ls` lists roots with their URLs while the
  listener runs. The daemon keeps the set of every object reachable from the public roots
  in memory (built at start, extended on add, rebuilt on remove) and answers `404` for
  any other CID exactly as for unheld content, on `/tree/`, `/blob/`, RASL and site
  origins alike. Nothing is fetched on demand (P4). `/at/` does not exist on this
  listener; `/api/` never does (P1).
- **Routes and behavior** are the loopback gateway's (`/`, `/tree/<cid>/<path>`,
  `/blob/<cid>`, `/.well-known/rasl/<cid>`, and `https://<cid>.<domain>/<path>` as the
  site origin): same non-active fallback on the bare origin, same types, caching, `Range`,
  redirects and listing CSP; the `Link: rel="alternate"` and "open as site" URLs use
  `https://<cid>.<domain>/`. RASL responses add `Access-Control-Allow-Origin: *` here
  (P5), so a page on any origin can fetch an object and verify it by hash (SHA-256 with
  WebCrypto; BLAKE3 needs a Nu-aware client). Only `GET`/`HEAD`; no cookies, no other
  CORS.
- **TLS.** `cert`/`key` are a PEM chain and key, loaded at start (a bad pair refuses the
  listener) and re-read within a minute of a change (renewals apply without restart; a
  failed reload keeps the last good pair). TLS 1.2+, HTTP/2 and HTTP/1.1. With
  `behind_proxy = true` the listener speaks plain HTTP and must be on a loopback
  address; the proxy terminates TLS for `domain` and `*.domain` and passes `Host`
  through. ACME is not built in (wildcards need DNS-01).
- **Refusals** (`invalid_argument`, action names the key): the node is a member of a
  private or isolated cluster (P3); no `cert`/`key` and no `behind_proxy`;
  `behind_proxy` off loopback; invalid `domain`.
- **Limits.** `max_requests` and `per_ip` bound concurrency (`503`, `Retry-After: 2`);
  `ReadHeaderTimeout` 10 s, idle 60 s, 16 KiB of headers. Behind a proxy `per_ip` sees
  the proxy's address.
- **Share.** `nu share <cid>[/path]` (and `PublicRoot`) carry `https` (the bare-origin
  URL: `/tree/<cid>/<path>` with a trailing slash for directories, `/blob/<cid>` for raw
  blobs), `site` (`https://<cid>.<domain>/<path>/`, directories only) and `rasl`
  (`rasl://<cid>/?hint=<domain>`) only while the public listener of the node answering
  the request serves the root (P2); otherwise the fields are absent. `nu status` shows
  `daemon.public` (the origin) when the listener runs.

## Safe extraction (`nu get -o`)

`node/extract.Extract(ctx, dest, walk)` writes a pre-order tree walk under `dest`:
paths must be relative with no empty, `.`, `..`, NUL or backslash segments and names ≤
255 bytes (else `invalid_argument`, nothing written); every existing path component under
`dest` is `Lstat`ed and a symlink anywhere on the way, or in place of the file being
written, aborts the extraction; symlink entries are never created and are listed in the
report; files are written to a temp file in the target directory and renamed into place;
modes are `0644`, or `0755` when the tree marks the file executable, `0755` for
directories. A name that already exists although this extraction did not write it, and
whose exact spelling is absent from the directory listing, has been mapped by the
filesystem onto another entry (case-insensitive APFS/NTFS, NFD-normalizing HFS+): it is
skipped and reported with the entry it collides with, and so is everything under a
skipped directory; a pre-existing file of exactly that name is replaced. The report
counts files, directories and bytes and lists skipped entries with reasons; the CLI
prints each one (rules X1–X6 of the [contract](operations.md#x-extraction-nu-get--o)).

## Daemonless commands

Content commands (`add`, `cat`, `ls`, `get`, `share`, `pin`, `public`, `dag`, `car`) try the daemon first (address
file, then a status ping). When none answers they take the repo lock, remove stale
address files, and run an in-process node (`node.Local`, which opens the store) for the
duration of the command, saying so on stderr; `add`, `pin add` and `dag put` add that the
content will be shared once a node runs (`nu daemon` or `nu share`), like `ipfs add`
offline. If another process holds the lock but its API does not answer, they fail with
`unavailable` and the action to wait or restart the daemon. `operations` requires the
daemon, since operations live in its memory.

## Targets

`get`, `cat`, `ls`, `share` and `dag get` parse their argument with `node/target` per
the [Targets table](../spec.md#user-experience): `nu://`, `at://`, `rasl://` links; a
`b…` DASL CID (`core/cid`: raw or DRISL, SHA-256 or BLAKE3, canonical lowercase base32)
with optional `/path`; `<handle-or-did>/<name>[/path]`; or `<dataset>[@<version>][/path]`.
A `nu://` link is parsed by `node/link` and yields a CID target with the link's hints
(`Target.Link`); a malformed link is `invalid_argument` with the parser's message. An
`at://` target is parsed by `atproto.ParseURI` (`at://<handle-or-did>[@<rev>][/<collection>[/<rkey>]]`,
with `@<rev>` as Nu's held-snapshot selector) and handled by the at:// commands above. A
A `rasl://<cid>/?hint=host…` target (parsed by `rasl.Parse`; the legacy
`web+rasl://<cid>;host,host/` form is accepted) yields a CID target whose hint hosts
become an HTTPS source for `get`, `cat` and `ls` ([D21](dasl.md#rasl)); the path is
ignored. A CID-length string that does not parse (for example a dag-pb CID) is
`invalid_argument` naming the accepted forms; so is anything else. A dataset target
resolves on the node to its head's root, or for `@<version>` to that version's root
([Version history](#version-history-0047)); `get` binds its operation target to the
resolved CID. Names are `not_supported` with an action.

## CLI

`nu [--json] [--nu-path <dir>] [--api <host:port>] <command> …`. `--json` is accepted
before or after the command and switches every command's stdout to JSON (errors stay on
stderr). The CLI reaches the daemon only through the `client` package (`node.Node` seam);
daemonless commands plug `node.Local` into the same seam.
