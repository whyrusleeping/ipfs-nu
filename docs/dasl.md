# DASL and atproto data

[Spec overview](../spec.md) · [Data](data.md) · [Names](names.md) · [Following](follow.md) · [Clusters](cluster.md) · [Operations](operations.md)

Nu's data model *is* DASL. Structure is core DASL (SHA-256 DRISL) and large raw blobs are
BDASL (BLAKE3). This document owns Nu's conformance to each DASL specification, the
atproto repository profile, and how Nu imports, exports, browses and archives atproto repos.
[Data](data.md) owns Nu's native blob/tree formats and transfer protocols; [names](names.md)
owns atproto identity and name records.

## D21. DASL conformance

"Full DASL support" means Nu can add, pin, transfer, serve, export and verify any valid DASL
or BDASL object without loss, and it produces only conformant objects. Application-layer
DASL formats are stored and served like any other content.

| Spec (dasl.ing status) | Nu support |
|---|---|
| CID (stable) | Native. DRISL structure uses SHA-256; any DASL CID is first-class |
| BDASL (stabilising) | Native for large raw blobs (BLAKE3, Bao verified ranges); BLAKE3 DRISL accepted |
| DRISL (stable) | Strict encoder/decoder; atproto `$link`/`$bytes` JSON is the JSON form |
| CAR (stable) | Verified import/export; full header metadata kept; atproto streamable order for repos |
| RASL (stabilising) | Gateway serves `/.well-known/rasl/<cid>`; `rasl://` links fetch from hints |
| MASL (stabilising) | File nodes are single-mode MASL documents; bundles pin, render and export |
| FISL (new, unstable) | Optional HTTPS discovery client; serving FISL is later |
| Web Tiles | MASL bundles/`.tile` CARs stored and exported; tile-isolated rendering is an extension |
| MUXL, PFP, S2PA | Content-agnostic: stored and served as blobs/DRISL; Nu does not generate or validate them |

Conformance claims name the DASL spec dates they target. CI runs the DASL test suite
([dasl-testing](https://github.com/hyphacoop/dasl-testing)) and the atproto interop vectors
([atproto-interop-tests](https://github.com/bluesky-social/atproto-interop-tests): data
model, MST, crypto, firehose). Spec revisions are tracked like other interop targets.
Unstable specs (FISL, Tiles protocols) may change without breaking Nu's own formats.

### CIDs and hash profiles

| Object | Hash | Verification and transfer |
|---|---|---|
| DRISL structure: trees, MST nodes, file nodes, records, commits, manifests | SHA-256 (DASL) | Whole object, ≤ 1 MiB for Nu-produced nodes |
| Raw blob, default (`nu add`) | BLAKE3 (BDASL) | Bao verified ranges, multi-peer, serve-while-downloading |
| Raw blob, `nu add --hash sha256`, atproto blobs, imported DASL blobs | SHA-256 (DASL) | Whole object, **≤ 8 MiB**, single request |

- Text form is `b` + lowercase base32; binary form is 36 bytes. Codecs are `raw` (0x55) and
  DRISL (0x71). Nu accepts all four DASL/BDASL combinations. It never produces BLAKE3 DRISL
  and never produces CIDv0, other hashes/codecs, identity multihashes or other multibases.
- Hashes are not aliased: the same bytes under SHA-256 and BLAKE3 are two CIDs. Nu does not
  record or trust cross-hash equivalences. It may store identical bytes once locally, but
  that is a storage optimization and never changes what a CID means.
- `--hash sha256` produces pure core-DASL content. A browser can verify it with WebCrypto,
  and any DASL tool (including atproto blob refs) can use it. It gives up Bao ranges, so it
  suits web/atproto-facing media rather than large archive files. It can be set per add,
  dataset or store default. Files over 8 MiB are refused and listed, never silently
  switched to BLAKE3. Measured costs are in [validation](validation.md).
- **Whole-object limit: 8 MiB (8,388,608 bytes).** Every object that can only be verified
  as a whole (all SHA-256 objects, BLAKE3 DRISL, and BLAKE3 blobs Nu fetches by known CID over
  plain HTTP) must fit. Importing a URL without a known CID is ingest rather than
  verification and is not limited for BLAKE3 ([D17](data.md#d17-store-and-replication)).
  This is a fixed protocol limit, not a per-node setting, so peers agree on it. Nu refuses
  larger such objects on add, import and transfer with an explanation. DASL itself sets no
  cap, and this is Nu's one documented conformance limitation. Large content uses BLAKE3
  blobs with Bao.
- **Whole-object transfer:** one request to one provider, buffered in bounded staging. On
  failure, restart from zero (no resume state). Bytes are exposed or served onward only
  after the hash matches. A mismatch discards the data and demotes the provider.
- Unsupported CIDs inside imported records (legacy or foreign CIDs in atproto leaf data) are
  kept byte-exactly as opaque links. They are reported but never followed, fetched or
  provided.

### DRISL

- The encoder emits only DRISL. The strict decoder rejects: non-string or duplicate map
  keys; non-canonical key order (length-first, then bytewise); non-minimal integers or
  lengths; indefinite lengths; tags other than 42; tag-42 values without the leading `0x00`
  or not a valid CID; simple values other than `false`/`true`/`null`; floats that are not
  64-bit or that are NaN, ±∞ or −0; and trailing bytes.
- Generic DRISL keeps 64-bit floats so that arbitrary DASL documents round-trip. Nu schemas
  contain no floats, and the atproto validation profile rejects floats as the atproto data
  model requires.
- Tag-42 links in Nu-native structures must be DASL/BDASL CIDs. The atproto import profile
  applies the opaque-link rule above only to record contents, never to commits or MST nodes.
- Limits: Nu-produced DRISL nodes are at most 1 MiB. Other DRISL objects up to the 8 MiB
  whole-object limit are stored, transferred and traversed (pins, walks, gateway rendering),
  with nesting-depth and memory limits.
- JSON (API, CLI, gateway): atproto conventions, `{"$link": "<cid>"}` and
  `{"$bytes": "<base64>"}`. `nu dag put` accepts that JSON and returns the SHA-256 DRISL CID;
  `nu dag get <cid>[/path]` returns JSON. Lexicon validation is not implied.
  `$link`/`$bytes` objects have exactly one key; bytes are standard base64, emitted
  unpadded and accepted with or without padding. JSON numbers without a fraction or
  exponent are integers, others are 64-bit floats; under the atproto profile an integral
  number such as `123.0` is the integer `123` and other floats are rejected.
- Opaque foreign links (atproto record contents only) must be syntactically valid CIDv1;
  their JSON form is `b` + base32. CIDv0 links are not accepted.

### CAR

- The reader accepts DASL CAR (v1). It hashes every block against its CID before storing,
  tolerates duplicates and arbitrary order, and exposes the full header metadata, not only
  `version`/`roots`. It does not store unreferenced blocks unless explicitly requested and
  reports them. CARv2 is rejected with an explanation.
- The writer emits DASL CAR. A CAR containing BLAKE3 CIDs (same 36-byte layout) is described
  as a BDASL CAR in output and docs.
- Deterministic orders: Nu tree pre-order (bundles, [D20](cluster.md#air-gap-bundles)) and
  atproto streamable order for repos (commit first, then MST nodes in pre-order with records
  interleaved in entry order).
- CAR headers may carry MASL metadata. Nu bundles put their manifest in the header under an
  `io.ipfs.nu` key alongside the required `version: 1` and `roots`.
- `nu car import <file>` / `nu car export <cid> [--profile …]` share the walk profiles below.

### RASL

Implemented in 0052 (`rasl/`, `gateway/rasl.go`, `fetch.Source`, `node/rasl.go`), against
the published spec (`rasl://<cid>/?hint=<host>…`).

- The gateway answers `GET`/`HEAD /.well-known/rasl/<cid>` for held DASL/BDASL CIDs, raw or
  DRISL, with the object's bytes as stored. Responses are `application/octet-stream` with
  `x-content-type-options: nosniff`, `Content-Disposition: attachment`, `ETag: "<cid>"`,
  immutable caching and `Range`; they are stateless (no cookies, credentials or content
  negotiation; the query string is ignored) and the only route is the exact
  `/.well-known/rasl/<cid>` on the bare origin. A public seed answers only for content it
  already serves publicly under the
  [exposure rules](operations.md#api-gateway-and-public-exposure). It never fetches unheld
  content on demand (an unheld CID is `404`), so it cannot act as an open proxy. The
  loopback gateway sends no CORS header (P5, N29); the public listener (0043) will add
  `Access-Control-Allow-Origin: *` there so browsers can verify by hash.
- `nu get|cat|ls rasl://<cid>/?hint=host[&hint=host…]` parses the URL per the spec (the
  CID is the authority; a `hint` is a DNS name or IP literal, optionally `:port`,
  lowercased, deduplicated, at most 16; invalid hints are dropped; the path is ignored).
  The legacy `web+rasl://<cid>;host,host/` form of `@dasl/rasl` 1.x is accepted on input
  and never produced. The hints become an HTTPS whole-object source ([D15](data.md#d15-content-provider-discovery))
  that the fetcher asks for every object of the walk, in parallel with Nu peers (the first
  verified copy wins); without networking the hints are the only source. Per object, all
  non-demoted hints are requested at once at `https://<hint>/.well-known/rasl/<cid>`, the
  first usable response takes the body and the others are aborted; if it fails mid-body
  or on verification, the hints that also answered are retried one at a time, and demoted
  origins are tried last. Any redirect is followed as a 307 (through `httpsrc`'s
  per-hop checks). Verification is by CID before anything is stored: SHA-256 objects and
  DRISL nodes within the 8 MiB whole-object limit, BLAKE3 raw blobs of any size (staged,
  then stored through the Bao-verifying `PutBlob`). Wrong bytes demote the final origin on
  the shared `httpsrc` client. Only 404/410 from every hint is `not_found`; other failures
  are `unavailable` with the per-hint causes. Fetching refuses loopback, private and
  link-local hints (checked again after redirects) unless `ingest.allow_ips` says
  otherwise, and an isolated cluster member refuses RASL fetches (no egress).
- `nu share` will emit a `rasl://` link with the public seed as hint once an HTTPS seed
  serves the object (0043); only SHA-256 objects are browser-verifiable without a Nu
  client. Interop with `@dasl/rasl` is by construction (same path, same headers, same
  first-success race); an automated cross-implementation run is not part of the tests.

### MASL

- [D3a](data.md#d3a-single-file-nodes) file nodes are single-mode MASL documents. `nu add`
  produces one; `--raw` returns the blob CID instead.
- A bundle-mode document (a `resources` map) is valid content. A recursive pin follows every
  `src`. The gateway serves `/` as the default resource with exact path matching (no implied
  directories) and ignores the query string. Bundles are rendered only on isolated origins,
  under the same rules as other active content.
- The gateway reflects only an allowlist of MASL HTTP headers (`content-type`,
  `content-language`, `content-disposition`, `content-encoding`), each validated.
  `content-security-policy` may only tighten the gateway's policy. Other headers,
  `service-worker-allowed` and manifest fields are ignored unless a later extension
  specifies them.
- `nu masl bundle <tree-cid>[/path]` exports a tree as a MASL bundle when it fits the DRISL
  limit, optionally as a `.tile` CAR (MASL header plus all resources). Nu trees stay the
  scalable native form, and bundles are an export, not a second tree model. If `src` CIDs
  are BLAKE3 the bundle is BDASL; `--hash sha256` content yields a core-DASL bundle.

### FISL

Configured FISL indexers are an optional HTTPS discovery source
([D15](data.md#d15-content-provider-discovery)). Location URLs are claims: Nu uses the
`https` URLs it supports, skips unknown schemes, honours expiry, applies RASL's fetch and
SSRF rules and verifies by CID. Answering FISL lookups from `nu relay` and defining a Nu
endpoint URL scheme are later work, done once the spec stabilises.

## atproto repository profile

Nu implements atproto repository format v3 exactly, on top of the same DRISL/CAR/MST code:

- **Commit:** `{did, version: 3, data, rev, prev: null, sig}`. The signature covers the
  SHA-256 of the unsigned DRISL commit. Keys are P-256 or secp256k1, from the DID document
  or a `did:key`.
- **MST:** SHA-256 key depth counted in 2-bit units (fanout 4); nodes `{l, e: [{p, k, v, t}]}`
  with mandatory prefix compression; canonical empty-node pruning. Parsing verifies depth,
  order and node-size/depth limits.
- **Keys:** `<collection>/<rkey>` for atproto repos. Cluster state repositories use the same
  NSID/record-key syntax.
- **Records** follow the atproto data model: no floats, and `blob` refs
  `{$type: "blob", ref, mimeType, size}` point to SHA-256 raw CIDs. Legacy blob refs are
  read and preserved.

One MST implementation serves both profiles. Depth hash, bits per level, node schema and
value form are parameters: the atproto profile uses links to records, while the
[Nu directory profile](data.md#d3-directory-trees) uses inline entries at fanout 16. The
atproto profile must match interop vectors byte-for-byte. Nu uses indigo's `atcrypto` (keys,
signatures) and `syntax` packages; its own MST, commits, proofs and diffs are cross-checked
against indigo's `atproto/repo` in the `experiments/` module, because that package pulls a
large IPFS module graph (including go-libp2p) into any module importing it.

Profile details (core/atrepo):

- **Nodes.** `l` and `t` are always present (`null` when absent) and must be SHA-256 DRISL
  CIDs; `v` is any DASL CID; `p` must be the full common prefix with the previous key (the
  first entry has `p: 0`). A link never skips a layer: a node at layer L links to layer L−1,
  through empty nodes holding only `l` where layers are missing. The root is never such a
  node; an empty node without `l` is only the root of an empty repo. Nodes are accepted only
  if they re-encode to the same bytes, so a tree that loads is the canonical tree of its keys.
- **Keys** are 1–1024 bytes when read. Records Nu writes use `<NSID>/<record key>` paths.
- **Commits** must carry exactly the six v3 fields (`prev` present, normally `null`), a
  valid DID and TID `rev`, and a signature of at most 128 bytes. Signatures are 64-byte
  compact low-S ECDSA over SHA-256 of the unsigned commit. Revs more than 5 minutes ahead of
  the local clock are refused. Keys come from a `did:key` or a DID document
  `publicKeyMultibase` (Multikey).
- **Limits on untrusted repos:** 256 entries per node (honest nodes follow a geometric
  distribution, P(≥ n) ≈ (3/4)ⁿ; the largest seen in real repos was 67), key layer ≤ 32,
  node ≤ 1 MiB, record ≤ 1,000,000 bytes, CAR blocks ≤ 1 MiB, 2²⁴ blocks per repo CAR and
  512 per record-proof CAR. Nu refuses to produce trees beyond the node limits as well.
- **Records** must be DRISL maps within the size limit. Records that break the atproto data
  model (for example floats, which exist in real repos) are kept byte-exactly and reported
  as non-conforming; they do not fail repo verification. Records Nu writes must conform.
- **Proofs** are what `com.atproto.sync.getRecord` serves: commit, the MST nodes on the key's
  search path (including empty intermediate nodes) and the record if present. A path that
  ends without the key proves absence.
- **CAR slices** (diffs and firehose `blocks`) carry the new commit, every MST node of the
  new tree that the old tree lacks, every node operation inversion reads, and created or
  updated records. Inversion of a `#commit` applies the ops in reverse (each must match the
  tree) over the slice alone and must reproduce `prevData`.
- **`getRepo?since=`** is applied by diffing the held verified tree against the new one over
  the held blocks plus the diff: every node that differs is validated, a subtree shared with
  the held tree is accepted only inside the key range and layer bound it had there, and
  every created or updated record must be present. A missing block means the chain broke;
  Nu then falls back to a full export.
- **Export** uses streamable order: commit, then each MST node followed by its slots in
  order (`l` subtree, then each entry's record and `t` subtree), each block once. Deployed
  PDSes do not emit this order, so a re-export keeps every block byte-identical but
  reorders the file and drops unreferenced blocks.

### Walk profiles

Pins, CAR export, `/nu/tree/1` and completeness marks share explicit walk profiles:

| Profile | Follows | Complete when |
|---|---|---|
| `nu-tree` | tree header → directory MST → entry metadata, file nodes → blobs | [D3](data.md#d3-directory-trees) |
| `atrepo` (`atrepo+blobs`) | commit → MST → records; blob refs only in `atrepo+blobs` | all structure and records present; blobs if selected |
| `drisl` | every tag-42 link, within bounds | every reachable block present |
| `direct` | nothing | the object itself is present |

Links out of an `atrepo` walk (other repos' records, unselected blobs) are expected to
dangle. They are reported and do not make the pin incomplete. The walk classifies blocks
by shape: a v3 commit links to `data`, an atproto MST node to its subtrees and records, and
anything else is a record whose links are not followed (with `atrepo+blobs`, its
`{$type: "blob"}` and legacy `{cid, mimeType}` refs to raw CIDs are).

### Using atproto data

```text
nu get at://<did|handle>/<collection>/<rkey>   # verified record → JSON (proof kept)
nu get at://<did|handle>                       # verified repo snapshot (getRepo CAR)
nu car import repo.car --profile atrepo        # offline: verify, store, pin
nu car export <commit-cid> --profile atrepo    # streamable-order CAR for atproto tools
nu ls at://<did>[/<collection>]                # list a held snapshot or live repo
```

- Record fetch reuses [D6](names.md#d6-resolution-and-caching). Resolve the identity, call
  `getRecord`, verify the commit signature and MST inclusion, then return the record with
  its commit CID and rev. Name resolution is this path specialised to `io.ipfs.nu.name`.
- Repo snapshots: `com.atproto.sync.getRepo` returns a CAR. Verify the commit signature
  against the DID document, rebuild the MST and check it equals `data`, validate every
  record block, then pin with `atrepo`. Blobs come from `listBlobs`/`getBlob` only when
  `--blobs` is set, verified by SHA-256 CID. Blobs over 8 MiB (for example long videos) are
  skipped and reported as oversized; they do not make the snapshot incomplete. Offline CAR
  import verifies structure, and
  verifies the signature once identity is available or when a signing key is pinned.
- Gateway: `/at/<did>[@<rev>]/<collection>/<rkey>` renders held records as JSON or plain
  text, never as active content. `/at/<did>` lists collections; blobs are served as
  non-active downloads unless isolated. Only held snapshots are served; the gateway never
  fetches a live repo on demand for public readers.
- Content commands work on atproto objects by CID like any other object: `cat`/`dag get` on
  record and commit CIDs, and `/nu/bytes/1` and RASL on repo blocks and blobs.

Decided in 0053 (mechanics in [api.md](api.md#atproto-records-snapshots-and-cars-0053)):

- A held snapshot is a recursive `atrepo` (or `atrepo+blobs`) pin of the verified commit
  named `at://<did>@<rev>`; browsing reads the held MST without re-verifying. Every
  `get at://<did>` adds a snapshot pin; older revs stay until unpinned (local history is
  private by default, as above). `@<rev>` in an at:// target or `/at/` path selects one.
- Record proofs are stored but not pinned: `get` of a record keeps the commit, path nodes
  and record re-verifiable until GC, without growing the pin set per record.
- `--blobs` takes blob CIDs from the verified records' refs (the `atrepo+blobs` walk),
  never from `listBlobs`, so every blob fetched is one a record names.
- Offline `car import --profile atrepo` verifies structure and reports the signature as
  unverified unless `--key` is given or the DID resolves; the import is still pinned, so
  the user decides what an unverified signature means for their data.
- The `drisl` profile follows every tag-42 link of held DRISL blocks (raw blocks end the
  walk); it is bounded by the store's held set and the CAR's 2²⁴-block limit. Exports under
  `nu-tree`/`drisl` refuse raw blobs over the 8 MiB whole-object limit, since CAR blocks
  are verified whole.

### atproto accounts as archive datasets

A cluster or one-node dataset may use an atproto account as its source (see
[D20 sources](cluster.md#datasets-and-updates)):

```text
source: { atrepo: "<did>", nsids?: [<nsid>…], blobs?: bool }
head:   { target: "<commit cid>", kind: "atrepo", rev, … }
```

- **Updates:** the same hint sources as [D19](follow.md#update-notification) (Jetstream with
  `wantedDids`, an infrastructure firehose, polling) prompt a fetch of
  `getRepo?since=<held rev>`. Nu verifies the commit and checks that applying the diff yields
  a tree equal to `data`. It falls back to a full export when the chain breaks, on `#sync`
  events or after a migration. A signed firehose `#commit` can be applied directly after
  MST-inversion validation.
- **Versions** are verified commits (by `rev`) in the dataset's version tree. `nsids` scopes which record collections count as the dataset. Unchanged records and MST subtrees are shared
  between versions.
- **Redistribution limits:** atproto expects mirrors to honour deletions and account status
  promptly and not to redistribute static snapshots in bulk. For accounts the dataset owner
  does not control, retained history is therefore private by default (local or
  cluster-only). Public serving tracks the current verified state only, and stops when
  Nu verifies the account is deactivated, taken down or deleted. Publishing retained
  third-party history needs an explicit per-dataset override that states this. Archiving an
  account you control carries no such default restriction.
- Records and repositories are untrusted input. Budgets bound records, MST node count and
  depth, blob bytes and request rates per account and per PDS host.

## Details still to specify

- Whether oversized atproto blobs (video) need an archive path. The 8 MiB whole-object
  limit currently skips them.
- `.tile` rendering headers. The MASL header allowlist and CSP-merge rule are G10 of the
  [API/browser security contract](operations.md#g-gateway-and-browsers); 0055 implements
  them.
- The verified HTTP range/proof mapping for BDASL (with bao-tree/iroh-blobs parity), shared
  with D14.
- `atrepo` dataset lifecycle: DID migration between PDSes, key rotation relative to retained
  commits (see [D6](names.md#d6-resolution-and-caching)), account deletion and
  per-collection retention.
- Whether firehose `#commit` blocks always carry complete inclusion paths
  ([D19](follow.md#details-still-to-specify)).

## References

- [DASL](https://dasl.ing/): [CID](https://dasl.ing/cid.html), [DRISL](https://dasl.ing/drisl.html),
  [CAR](https://dasl.ing/car.html), [BDASL](https://dasl.ing/bdasl.html),
  [RASL](https://dasl.ing/rasl.html), [MASL](https://dasl.ing/masl.html),
  [FISL](https://dasl.ing/fisl.html), [Tiles](https://dasl.ing/tiles.html),
  [IPFS considerations](https://dasl.ing/ipfs.html)
- [DASL test suite](https://github.com/hyphacoop/dasl-testing)
- atproto [repository](https://atproto.com/specs/repository), [data model](https://atproto.com/specs/data-model),
  [sync](https://atproto.com/specs/sync), [blob](https://atproto.com/specs/blob)
- [atproto interop tests](https://github.com/bluesky-social/atproto-interop-tests)
- [indigo atproto packages](https://github.com/bluesky-social/indigo/tree/main/atproto)
