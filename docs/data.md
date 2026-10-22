# Data, transfer and storage

[Spec overview](../spec.md) · [Names](names.md) · [Networking](networking.md) · [Following](follow.md) · [Clusters](cluster.md) · [Operations](operations.md)

## D1. CIDs

- CIDv1; text form is lowercase base32.
- Codecs: `raw` (0x55) and DRISL / `dag-cbor` (0x71).
- **Structure** (every DRISL node Nu produces): SHA-256 (0x12), core DASL, as in atproto.
- **Raw blobs:** BLAKE3 (0x1e, BDASL) by default, for Bao verified ranges; SHA-256 raw blobs
  (`--hash sha256`, atproto/DASL imports) are first-class but only verified as a whole,
  so they are limited to 8 MiB like every whole-object-verified object.
- Nu-produced DRISL blocks are at most 1 MiB.

Nu accepts every DASL and BDASL CID and produces nothing else. Hashes are not aliased; the
same bytes under two hashes are two CIDs. Conformance, limits and the atproto profile are
in [D21](dasl.md#d21-dasl-conformance). DRISL is a strict subset of DAG-CBOR, but IPLD
compatibility does not imply IPFS data-transfer compatibility: whole-file blobs can exceed
Bitswap's approximately 2 MiB block limit.

## D2. Blobs

A blob is `CIDv1(raw, blake3(whole file))` by default: identical bytes always have the same
blob CID, independent of filenames or import settings other than the hash choice. Bao
applies only to BLAKE3 blobs. SHA-256 blobs are fetched and verified whole, as
[D21](dasl.md#cids-and-hash-profiles) specifies.

Store a Bao outboard with **16 KiB chunk groups** (16 BLAKE3 chunks), approximately 0.4%
space overhead. Use `lukechampine.com/blake3/bao`; cross-check encoding vectors against
`bao-tree` at the same group size. BLAKE3 roots match iroh-blobs, but Nu uses its own
transfer protocol and does not depend on iroh-blobs.

- The outboard is a standard Bao outboard at group log 4: the blob size as 8 bytes little
  endian, then each parent node (left and right chaining values, 64 bytes) in pre-order.
  Blobs of at most 16 KiB have no outboard and are verified whole.
- Ranges are half-open chunk-group ranges `[start, end)`. A range is sent as a standard Bao
  slice (size header, then the parents on the path and the range's groups in pre-order),
  checked group by group as it arrives; receivers keep the parents they verify, so they can
  serve the groups they hold.
- A byte range `[a, b)` maps to groups `[⌊a / 16384⌋, ⌈min(b, size) / 16384⌉)`. An empty or
  past-the-end byte range maps to the single group holding its (clamped) start.

Downloads support independently verified ranges, resume and multiple providers. Verify data
before exposing or serving it; drop peers that supply invalid data. Completed ranges can be
served onward before the whole blob is present.

## D3. Directory trees

Each directory is a canonical MST mapping **entry name → entry**, using the same structure
at every size, with no sharding threshold. The selected fanout is **16**, to be validated by
benchmarks and cross-implementation vectors. Node sizes vary and remain subject to D1's cap.
It is the Nu directory profile of the shared MST implementation: SHA-256 key depth counted
in 4-bit units, SHA-256 node CIDs and inline entries. The atproto profile is the other
([D21](dasl.md#atproto-repository-profile)).

Per-directory trees preserve shared-subdirectory deduplication and allow moves by relinking
subtree CIDs. Sorted MST keys support listing, prefix queries and name-range pagination.
Expected lookup and update cost is O(log n); full listings remain O(n). A path lookup follows
the MST search path in each directory, not every entry in those directories.

```text
entry: { kind: "file"|"dir"|"symlink", cid, size, mode?, mime?, meta?: <cid> }
header: { $type: "io.ipfs.nu.tree", v: 1, root: <dir cid>, files, bytes, collection? }
```

`size` describes total bytes below an entry for progress and quota planning. Files in a
directory point directly to raw blobs; their entries carry the metadata. A directory can
span multiple MST nodes. Tree transfer streams path proofs in one request and skips shared
subtrees during updates.

**Encoding (nu-dir profile, v1).** A key's layer is the number of leading zero bits of
SHA-256(name) divided by 4. The tree for a set of names is one node holding, in name order
(bytewise), the names of the highest layer present, with the trees of the remaining names
in each gap before, between and after them; empty gaps have no subtree. No empty nodes exist
except the root of an empty directory, so the root CID depends only on the entries. Nodes
are SHA-256 DRISL:

```text
node:  { e: [ { k: <name>, v: <entry>, t?: <right subtree cid> } … ], l?: <left subtree cid> }
```

Decoders accept only the canonical encoding (no unknown keys) and check on every load that
names are sorted, share one layer below the parent's, and lie inside the parent gap. A node
over 1 MiB is not produced: the add fails and names the directory. Vectors are in
`testdata/vectors/nu-tree/`.

**Entries.** Names are non-empty UTF-8 of at most 255 bytes without `/` or NUL, not `.` or
`..`, kept byte-exact (no Unicode normalization); others are refused on import with a
"rename or exclude" message. Devices, sockets and FIFOs are skipped and reported.
- `file`: `cid` is the raw blob, `size` its length. `mode` is `0o755` when the owner-execute
  bit is set and absent otherwise; other permission bits and times are not recorded, so
  trees are reproducible across umasks.
- `dir`: `cid` is the directory's MST root; `size` is the sum of its entries' sizes.
- `symlink`: `cid` is a raw blob of the target bytes, `size` their length. Symlinks are
  recorded, never followed.
- **Platforms.** Names are read from the filesystem as stored and never normalized: macOS
  APFS preserves the spelling a file was created with (HFS+ stored NFD), so the same
  visible name created on Linux and on macOS can be two different entries. Extraction
  writes names byte-exact; where the destination folds case or Unicode spellings (APFS,
  NTFS, casefolded ext4) the second spelling is skipped and reported naming the entry it
  maps onto ([X4](operations.md#x-extraction-nu-get--o)); nothing is merged or renamed.
  Symlinks are recorded on import and reported, not created, on extraction (X3);
  ownership, other permission bits, times, xattrs, ACLs, resource forks and Finder
  metadata are not recorded, so they are not transformations. Tests:
  `node.TestFilenameRoundTrip`, `extract.TestExtractSafety`.
- The importer sets no `mime` (type guessing is machine-dependent). In the header, `files`
  counts file entries in the whole tree and `bytes` is the root directory's `size`.
- nu-tree walk and CAR pre-order: header → root; directory node → left subtree, then per
  entry its target, its `meta` and its right subtree; file node → `src`.

`meta` links a file or directory entry to an application metadata record: generic SHA-256
DRISL of at most 64 KiB, such as a STAC item subset or a catalogue record. Metadata is
structure. It is walked, pinned, transferred and diffed with the tree, and a metadata change
is an entry change. Nu does not interpret metadata except through the facets that a
`collection` header declares ([D22](collections.md#items-and-facets)).

## D3a. Single-file nodes

`nu add photo.jpg` returns a small single-mode [MASL](dasl.md#masl) document:

```text
{ $type: "ing.dasl.masl", src: <raw blob cid>, content-type?,
  "io.ipfs.nu": { size, name?, mode? } }
```

The wrapper supplies download/gateway metadata, so DASL tools can read it without knowing
Nu. Different names can produce different file nodes referring to the same blob.
`nu add --raw` returns the blob CID instead. Tree entries keep their own `mime`; the
gateway applies the MASL header allowlist in D21.

A `/nu/tree/1` response for a file node with `scope: entry` carries the node and inlines
its blob when the blob fits in one 16 KiB group. Larger blobs are never streamed in tree
responses: the verified node or entry authenticates their CID and size, and they are fetched
over `/nu/bytes/1` (D14).

### Type discovery

The CID codec distinguishes raw bytes from structure; `$type` distinguishes structured
node kinds (`ing.dasl.masl` for file nodes and bundles, `io.ipfs.nu.tree`, atproto commits
by shape), and walk profiles ([D21](dasl.md#walk-profiles)) decide which links to follow.
Directory entries and name records carry `kind`/`size` hints, and `/nu/have/1`
reports `{codec, size}`. Every object can also be fetched as bytes by hash without
understanding its schema.

## D14. Transfer protocols

Binary protocols on libp2p streams have two layers: bytes by hash is sufficient for
correctness; tree sync is an optional accelerator. Names travel through atproto instead.

### `/nu/bytes/1`

- Request: `{mh, ranges?}`. `mh` is the 34-byte binary multihash (SHA-256 or BLAKE3); the
  codec does not affect the bytes. `ranges` (BLAKE3 only, at most 64) is a list of
  `[start, end]` half-open 16 KiB chunk-group ranges ([D2](#d2-blobs)), non-empty, sorted
  and disjoint. Without `ranges` the request is for the whole object.
- Response header: `{status, error?, size?}`; `size` is the object's byte length and is
  present when `status` is 0. Then the body, then FIN:
  - SHA-256: exactly `size` plain bytes, verified at the end
    ([D21](dasl.md#cids-and-hash-profiles)).
  - BLAKE3: one standard Bao slice (D2) per requested range, in request order, each with its
    own 8-byte size header; without `ranges`, the slice of the whole blob. Verified group by
    group as it arrives.
- A server serves what its store holds: complete objects, and for partial BLAKE3 blobs the
  groups it has verified and synced. If any requested range is not wholly held, or a partial
  blob is requested whole, it answers status 2 without a body. Ranges outside the blob get
  status 3.
- Whole-object-verified objects (SHA-256 of any codec, BLAKE3 DRISL, and BLAKE3 blobs of at
  most one group) are fetched whole in one request: the client refuses a `size` over 8 MiB
  before reading the body, buffers it, and stores or serves it only after the hash matches.
  Larger BLAKE3 blobs have no size cap and are fetched by ranges into a partial blob, which
  keeps every verified group and serves it onward. D1's structured-block cap still applies
  to Nu-produced nodes.
- Statuses: 0 ok; 1 not found (no part of the object held); 2 not held (partial blob
  without every requested group; ask `/nu/have/1`); 3 bad request (malformed, invalid
  ranges, over a limit); 4 busy (concurrency limit; retry later or elsewhere); 5 internal
  error. Other values are refusals. Refusals have no body; `error` is human-readable text.
- A peer is **corrupt** if data fails verification, a header is malformed, oversized or
  lacks `size`, `size` differs from a size the client knows, a whole object exceeds 8 MiB,
  or bytes follow the expected body. The client discards unverified data and demotes the
  peer. Refusals, resets, truncation and timeouts say nothing about a peer's honesty.
- A minimal client can fetch every Nu object with this protocol and walk DRISL links itself.
- The fetcher divides large BLAKE3 blobs across peers in 1 MiB chunks (one range request
  each, per-peer concurrency bounded), planned from `/nu/have/1`, and re-requests outstanding
  chunks from a second peer during the endgame; the first verified copy wins. A peer that
  sends corrupt data is dropped for the rest of the fetch; peers that lack data stay.
  Serving reads blob data and its outboard.
- Blob sizes come from an authenticated parent (tree entry, file node). Otherwise a size
  claimed in `/nu/have/1` is proven before use by verifying the blob's last group from a
  peer that claimed it (Bao binds the size to the root). A partial blob whose size differs
  from the proven size is discarded, and peers that claimed another size are dropped.
- Without `/nu/tree/1`, trees are fetched by a layer-1 walk: the nu-tree walk's structure
  nodes level by level in parallel batches, then the blobs in parallel.

### `/nu/tree/1`

```text
{ root, profile?, path?: [name…], name_range?: [start, end], limit?, scope: node|entry|subtree,
  have?: [roots], inline_max?: bytes, cursor?: {path?: [name…], key?, link, after?} }
{ nodes: [cid…], scope: node, limit?, inline_max? }
```

- `root` is a tree header, a directory MST root or a MASL file node. `profile` selects the
  walk and defaults to `nu-tree`, the only profile so far; others get status 3. `path`
  names the target below the root directory, `scope` how much of it to send:
  - `node`: the path chain and the target directory's root MST node;
  - `entry`: the chain, then for a directory all of its MST nodes (a listing; entries' meta
    records and inline blobs, no subdirectories), for a file or symlink its inline blob,
    and the target entry's meta record;
  - `subtree`: everything the nu-tree walk reaches below the target.
- The response header is `{status, error?}` as for `/nu/bytes/1`; a root the server does not
  hold gets status 1. The body is a sequence of frames `<uvarint len> <tag> <payload>`, each
  at most 1 MiB + 1 byte, then FIN:
  - `0` end trailer, DRISL `{more?: true}`, always last;
  - `1` data: the bytes of the next item, a structure node or an inline blob;
  - `2` have: the next item is in one of the `have` trees; it is not descended;
  - `3` missing: the server does not hold the next item; it is not descended.
- Items follow one fixed pre-order: the root; for a header its root directory; per MST node
  the node, its left subtree, then per entry its target (subdirectory or inline blob), its
  meta record and its right subtree. Along `path` only the search path of each name is
  walked; it proves presence or absence. Each item is sent once per response (a repeated CID
  is skipped). Both sides run the same walk, so every frame is checked against the CID its
  verified parent links: a hash mismatch, an inline blob over `inline_max`, an unknown tag,
  a payload on a marker or a frame after the end is corrupt. MST nodes are also checked
  against their parent's key range and layer.
- `have` lists up to 16 tree headers or directory roots of complete trees the client holds.
  The server walks them side by side with the requested tree, by name, and sends a have
  marker instead of an entry target or meta record equal to the old entry's, and instead of
  an MST subtree that lies on the old directory's search path for its first name. Have
  roots the server lacks are ignored, so it sends more, never less. The client descends
  nothing behind a marker. If no peer can diff, it walks itself with `nodes` requests.
- `name_range` (`[start, end)`, `""` open; needs `scope` entry or subtree) restricts the
  target directory's entries. The walk still sends every MST node whose key range meets the
  range, which proves the listing complete at its boundaries.
- `inline_max` (capped at 16 KiB) inlines file and symlink blobs whose entry size is at most
  that, checked by hashing them whole. Larger blobs are fetched over `/nu/bytes/1`.
- Budgets: `limit` caps data frames; servers also stop at their own frame/byte budget
  (defaults 4096 frames, 16 MiB) and then end with `{more: true}`. Every response carries at
  least one data frame.
- Cursors: every item has a location, the link that reached it: `link` 0 the root, 1 the
  root MST node of directory `path`, 2 the left subtree of the MST node whose first entry is
  `key`, 3 the blob of entry `key`, 4 its meta record, 5 its right subtree, 6 the blob of a
  MASL root. `path` is the directory from the root directory, empty for links 0 and 6; `key`
  is absent for 0, 1 and 6. A location is unique in a tree. The client's cursor is the
  location of the last item it handled, with `after` when that item was a blob or marker
  (resume after its subtree) rather than a received node (resume with its descendants). A
  repeated request with the cursor resumes at the next item at any peer holding the tree:
  the server seeks along the cursor's path without sending, and the client follows it
  through its own verified nodes. Nothing verified is lost and no item before the cursor
  is sent again (a CID that recurs later in the tree may be); a cut frame is resent. A cursor that is not a location of the walk gets status 3, and one whose
  path the server does not hold gets status 1.
- `nodes` (at most 256 CIDs) returns each in order as a data or missing frame (raw blobs up
  to 16 KiB inline). A budget stop answers a prefix; the client asks again for the rest.
  Publishers and seeds retain recent targets' structure so they can answer `have` diffs
  directly ([D19](follow.md#delta-sync)).
- Walk profiles: `atrepo` (commit → MST → records, `path`/`name_range` as repo-path keys,
  held commits as `have`) is the next profile; locations of its layered empty intermediate
  nodes are specified with it. Atproto datasets sync by `/nu/bytes/1` walks until then
  ([D21](dasl.md#walk-profiles)); cluster state repositories use repo diffs on
  [`/nu/cluster/1`](cluster.md#nucluster1).
- The fetcher asks peers for `scope: subtree` with 16 KiB inlining first, resuming at the
  next peer from the cursor on failure, and finishes with the layer-1 walk, which fetches
  large blobs and anything the tree responses did not deliver. Peers without the protocol
  are skipped.

### `/nu/have/1`

Request: `{want: [{mh, range?}, …]}` with 1 to 256 items. `range` (BLAKE3 only) is the
`[start, end]` chunk-group window to report for a partial blob, by default from group 0; an
`mh` may repeat with different windows.

The response is a single DRISL message with no body: `{status, error?, have?, info?}`.
`have` is a bitmap of ⌈n/8⌉ bytes for n items: bit i is bit `i mod 8` (least significant
first) of byte `i / 8`, set if item i is held completely or partially; padding bits are 0.
`info` has one entry per set bit, in item order:

```text
{ codec, size, complete?: true, window?: [start, end], ranges?: [[start, end]…], bits?: bytes }
```

`codec` is the codec the object was stored under and `size` its byte length. Complete
objects set `complete` only. A partial BLAKE3 blob reports its held groups within `window`,
the requested window clamped to the blob and to 32768 groups: at most 32 held ranges as
`ranges` (absolute, sorted, disjoint), otherwise `bits`, a bitmap over the window (bit j is
group `start + j`, same bit order). With neither, nothing in the window is held. Statuses
and refusals are as for `/nu/bytes/1`.
Send only to candidate peers (name context, providers or hints), not to every connected peer.

### Framing and limits

```text
→ multistream-select: /nu/bytes/1
→ <uvarint len> DRISL {mh, ranges: [[0, 64]]}
← <uvarint len> DRISL {status: 0, size: …}
← body: Bao slices, or /nu/tree/1 frames; FIN
```

Use optimistic multistream-select and one stream per request: the client sends the
proposal and the request together, then half-closes. Requests and response headers are DRISL
maps with minimal uvarint length prefixes. Ignore unknown keys; breaking changes require a
new protocol ID. Either side cancels with a reset; QUIC per-stream flow control supplies
backpressure. A server that fails mid-body resets the stream.

Fixed limits: requests ≤ 16 KiB, `/nu/bytes/1` and `/nu/tree/1` headers ≤ 4 KiB,
`/nu/tree/1` frames ≤ 1 MiB + 1 byte, `/nu/have/1` responses ≤ 2 MiB. Larger messages are protocol violations (servers answer status 3). Servers bound
concurrent requests per protocol (default 64) and per peer and protocol (default 16) with
status 4, read the request within 10 s and reset a response the peer has not read for 30 s;
these defaults are local policy. Vectors are in `testdata/vectors/nu-proto/`.

Convenience HTTP/HTTPS gateway reads, listings/downloads and `Range` arrive in the sharing
track; ordinary browsers trust the serving gateway/TLS. The gateway also answers RASL
(`/.well-known/rasl/<cid>`, [D21](dasl.md#rasl)); SHA-256 objects fetched that way are
browser-verifiable with WebCrypto. A separately specified verified
HTTP mapping (`GET /bytes/<cid>` and tree/proof responses) must define range/proof framing,
aligned with BDASL's range rules, before Nu-aware HTTP clients claim independent Bao
verification. Browser-native p2p is not
required for first browser access. IPFS import is a later [verified one-way bridge](migration.md);
Nu-native content is not served over Bitswap. Graphsync is not used.

## D15. Content-provider discovery

Try sources in this order:

1. Link hints (`peer=`, `via=` → `nu.node`).
2. Publisher nodes from the name context, seeds first.
3. For followed names, the [dataset swarm](follow.md#dataset-swarm), found through its swarm
   key and checked via `/nu/have/1`.
4. Candidate connected/LAN peers via `/nu/have/1`.
5. Amino DHT provider records.
6. Delegated routing: `GET /routing/v1/providers/{cid}`.

Cluster members fetching cluster datasets instead use [D20 placement](cluster.md#placement):
owners by rank, surplus holders, then other members. Private and isolated clusters never use
steps 5–6.

HTTPS sources complement libp2p providers. RASL hints from links (`fetch.Source`,
implemented by `rasl.Source`, asked for each object alongside the peers; [D21](dasl.md#rasl)),
and configured FISL indexers (`fisl.indexers`, `fisl.Source`, asked for each object the
same way; [D21](dasl.md#fisl)), yield URLs that are fetched statelessly and verified by
CID through one retrieval path (`rasl.Retriever`). Private and isolated clusters do not query public
indexers; isolated members make no RASL fetches.

Announce pinned blob, file-node and tree-root CIDs through Amino and, if configured, delegated
routing; never announce interior directory shards. Amino provider keys use each CID's native
multihash bytes: SHA-256 keys for structure and BLAKE3 (0x1e) keys for blobs. A bounded live
check retrieved random BLAKE3 blob providers from an independent Amino client; do not derive a
SHA-256 routing alias or use a separate DHT protocol. It did not verify the deterministic
follow-swarm key, so DHT-based swarm discovery is best-effort/unverified; do not rely on it in
place of link hints, publisher context or LAN/member discovery. Follow pins default to
announcing only the swarm key and current root (`follow.provide`, D19). A wrapped standalone
file announces both wrapper and blob; providing scales with files/roots rather than transfer
chunks. Provider records do not say which transfer protocol the provider serves. See
[validation](validation.md#q2-amino-provider-keys-and-citizenship-bounded-live-results-incomplete-swarm-run).
Hint/LAN/explicit-member retrieval does not depend on Amino; a routing fallback is not a second
content model.

## D17. Store and replication

- Store BLAKE3 blobs as files with Bao outboards and SHA-256 blobs as files without one;
  small blobs may be inline. Objects are keyed by multihash. `--nocopy` references source
  files in place.
- **URL import:** `nu add https://…` and collection item pushes with URL files
  ([D22](collections.md#ingest)) fetch over HTTPS with `httpsrc`'s SSRF and
  redirect guards. They hash on arrival and store the result as a local add would. This is
  ingest, not verification. The CID names the bytes received, and trust in the source rests
  on TLS plus any checksum the caller supplies. Resume uses `Range` with `If-Range` on a
  strong validator; without one, the download restarts. BLAKE3 URL imports have no size
  limit, while `--hash sha256` imports keep the 8 MiB whole-object limit. The add result
  records the URL, validators and size as provenance, and `--meta` can carry them into a
  tree entry. *Implementation (0059):* `httpsrc.Import` streams into a retained file, then
  hashes it into the store (with any checksum verified in the same pass); `nu add` retries
  up to 4 times within its operation, and item pushes keep the retained bytes and
  validators under `cluster/net/ingest/`, so a restart resumes them. Adopting the retained
  file into the store without a second copy, and charging it to `store.limit` while it
  downloads, are hardening work.
- Use embedded SQLite (`modernc.org/sqlite`, pure Go) for DRISL blocks, pins, name cache,
  follow/dataset state, collection indexes and peer/address caches; blobs and outboards are
  files. The schema carries a version and forward migrations from the first release. Files
  in `$NU_PATH` have mode `0600`.
- Local layout (schema version 1, `PRAGMA user_version` with forward migrations): raw
  blobs of at most 16 KiB and DRISL blocks live in the database; larger blobs are files
  `blobs/<xx>/<multihash hex>` with the outboard beside them as `.obao`. Writes stream
  through `staging/` and are synced and renamed into place before their row commits, so
  objects appear atomically and a crash leaves only orphan files, which GC removes. A partial
  BLAKE3 blob keeps sparse data and outboard files plus a bitmap of verified groups that is
  saved only after the data is synced; fetching resumes from it after a restart.
- `--nocopy` blobs store only the outboard and the source's path, size and mtime. Each use
  checks size and mtime; on a mismatch the file is re-hashed, and if it no longer matches
  its row is dropped, so it is neither advertised nor served until re-added or fetched.
- Pins are recursive or direct and tagged by source: explicit, follow, my-names, cluster,
  cluster-surplus ([D20](cluster.md#sync-handoff-and-repair)), or collection-select,
  collection-manual or collection-conflict ([D22](collections.md#selection)). Recursive pins name a walk
  profile (`nu-tree`, `atrepo`, `drisl`; [D21](dasl.md#walk-profiles)) that defines which
  links count and when the pin is complete.
  GC is mark-and-sweep, triggered by the watermark, free-space reserve and admission
  shortfalls (A7); unpinned objects younger than a grace period survive so in-progress
  adds and fetches are not collected before they are pinned. Explicit releases (unfollow,
  unpin, collection release) may also trigger an incremental sweep of the released content.
- `nu follow` pins newly resolved targets according to policy. Retain the previous pinned
  target as a replication safeguard, not as publication history. Large, frequently updated
  trees use delta sync, completeness marks, atomic pin switches, scopes and quotas
  ([D19](follow.md)).
- Seed nodes follow their owner's DID under specified lifecycle/budgets. Laptop publication
  → exact-target seed completion wait → laptop sleep is the intended workflow; a fresh third
  client must retrieve the acknowledged CID. Proof/publication alone is not readiness.
- Quota, headroom, staging and pressure rules are the accounting rules below and the
  [capacity contracts](operations.md#capacity-and-transfer-budgets).
  Pins needed by held views, retained archives or unconfirmed handoffs survive disk pressure;
  GC reclaims only unpinned content. `--nocopy` is not an independent archive copy: source
  mutation/deletion must invalidate availability until reverified or reacquired.
- Linux/macOS import/extraction semantics are in [Entries](#d3-directory-trees) (Platforms) and
  [X1–X6](operations.md#x-extraction-nu-get--o); content export and clean-machine restore
  in [B1–B8](operations.md#b-backup-export-and-recovery).

### Accounting and budgets

Rules are numbered so tests and status output can cite them. The store (`store`) implements
A1–A8; the transfer rules are in [operations](operations.md#capacity-and-transfer-budgets).

- **A1 Three measures, never mixed.** *Logical bytes* of a target (a pin, dataset or
  follow) are the sizes of the distinct objects its walk profile reaches, each counted once
  per target: a raw blob by its length, a structure block by its encoded length. The same
  object under two targets counts in each. `max_size`/`--max-size` are logical. *Physical
  bytes* are what the store occupies on disk, daemon-wide, each object charged once however
  many pins reach it: complete copied blobs, outboards (`blob.OutboardSize`), the verified
  bytes of partial blobs plus their outboards, the database file with its WAL (blocks, pins,
  indexes) and in-progress staging. *Transfer bytes* are bytes received over an interval
  and belong to the fetch/follow layer, not the store.
- **A2 Claims are not sizes.** A size from a tree entry, file node, name record or peer is a
  claim until the object is held and verified. The store reports held logical bytes and
  the count of missing objects (`WalkStats`); a preflight that adds declared sizes of
  missing objects reports them as an estimate with that uncertainty. Physical bytes are
  charged as they verify (a partial blob by its verified groups), never as declared.
- **A3 `--nocopy`.** A referenced source is not charged (only its outboard is); its bytes are
  reported separately as `nocopy`. Once a source fails verification its row is dropped, so
  it is neither held, logical nor advertised until re-added or fetched.
- **A4 Attribution.** A *scope* is a pin source plus name: one follow, dataset, collection,
  or the explicit pins (`explicit`, name `""`). An object's physical bytes are *unique* to a
  scope when only that scope's pins reach it, *shared* when pins of two or more scopes reach
  it, and *unpinned* when no pin reaches it (grace-protected adds and fetches, cache,
  abandoned partials). `unique` is what releasing the scope frees after GC; releasing a
  scope never frees shared bytes. Per-scope `logical`, `physical` (everything its pins
  reach) and `unique` come from one mark pass over all pins (`Store.Attribution`).
  *Example:* follows F1 and F2 each pin a 10 GiB tree; 6 GiB of blobs are identical.
  Logical: 10 GiB each. Physical daemon-wide: 14 GiB plus outboards. F1 unique 4 GiB, F2
  unique 4 GiB, shared 6 GiB. `nu unfollow F1` frees 4 GiB. A retained previous version of
  F1 that differs by one 1 GiB file adds 1 GiB physical, unique to F1 (the scope), and 0
  logical to F1's current target.
- **A5 Budgets.** `store.limit` is the physical budget (0: unlimited); `store.free_reserve`
  is free space kept on the filesystem holding the store (default 1 GiB); headroom is
  `min(limit − physical, free − reserve)` over the terms that apply. `store.gc_watermark`
  (default 0.9) is the fraction of `limit` above which automatic GC runs.
- **A6 Admission.** Every write declares its need before it stores anything: a known-size
  add or fetch needs the object plus its outboard; a partial blob needs its missing groups
  plus the outboard on first open; an unknown-size add needs one chunk group up front and
  its actual bytes at commit. When need exceeds headroom the store first reclaims (A7),
  rechecks, then refuses with `BudgetError{reason: store_limit|free_space, need, headroom,
  physical, limit, free, reserve}`; the API reports it as `insufficient_storage` with those
  details and the action. A refused write leaves nothing visible and nothing held is
  released. Partial blobs are refused before any byte is fetched when the declared size
  exceeds headroom or `MaxBlobSize` (8 TiB, the bound of the group bitmap); the fetcher
  proves a claimed size by verifying the last chunk group before fetching the rest and
  discards partials of disproven sizes.
- **A7 Reclaim and pressure.** GC runs when physical ≥ watermark × limit, when free space
  < reserve, on an admission shortfall (at most once a minute), and on request. It removes
  only unpinned objects older than the grace period, abandoned partials (unreachable from
  every pin and not updated within the grace), and orphan files; `staging/` is emptied at
  open. GC never removes pinned content of any source: under pressure, acquisition pauses
  with the bytes needed and the action (release pins with `pin rm`/unfollow, free disk
  space, or raise `store.limit`); protected content ([S7](operations.md#s-safe-release))
  is never released to make a new update fit.
- **A8 Status.** `nu status` and `GET /api/v0/status` report the `store` section:
  `{objects, pins, bytes (physical), blobs, outboards, partials, db, staging, reserved
  (admitted, unwritten fetch bytes), nocopy, free, limit, reserve, headroom, pressure}`,
  where `free`/`headroom` are `-1` when unknown or unlimited and `pressure` is set while
  headroom is 0. Operations that stop on a
  budget fail with `insufficient_storage`; a follow or cluster consumer that pauses instead
  reports the same details in its paused status.

## Details still to specify

The design above selects the format and protocol families, not yet every wire byte. Remaining
precision work includes the `atrepo` profile of `/nu/tree/1`. Name kinds now distinguish `tree|file|blob`; canonical
vectors and wrapper/stream behavior still require implementation validation. Accounting is
specified (A1–A8, [T1–T4](operations.md#capacity-and-transfer-budgets)); transfer budgets
are enforced when their first consumer (follow) lands. Crash-consistent pin/held switches remain
to specify; content export and upgrades are [B1–B8 and U1–U6](operations.md#b-backup-export-and-recovery). Benchmarks must validate
fanout/node-size behavior and actual archive update/storage costs.

## References

- [DASL / DRISL / BDASL / RASL / MASL](https://dasl.ing/); Nu conformance in [D21](dasl.md)
- [Bao Go package](https://pkg.go.dev/lukechampine.com/blake3/bao)
- [bao-tree](https://docs.rs/bao-tree/latest/bao_tree/)
- [iroh-blobs protocol](https://docs.rs/iroh-blobs/latest/iroh_blobs/protocol/)
- [IPFS trustless gateway](https://specs.ipfs.tech/http-gateways/trustless-gateway/)
- [Delegated routing](https://specs.ipfs.tech/routing/http-routing-v1/)
