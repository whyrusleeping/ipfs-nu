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

- The gateway answers `GET`/`HEAD /.well-known/rasl/<cid>` for held DASL/BDASL CIDs, raw or
  DRISL. Responses are `application/octet-stream` with `x-content-type-options: nosniff`,
  stateless (no cookies, credentials or content negotiation) and `Range`-capable. A public
  seed answers only for content it already serves publicly under the
  [exposure rules](operations.md#api-gateway-and-public-exposure). It never fetches unheld
  content on demand, so it cannot act as an open proxy.
- `nu get rasl://<cid>/?hint=host` tries the hints in parallel over HTTPS, alongside normal Nu
  discovery. It treats any redirect as a 307 and verifies by CID, within the 8 MiB
  whole-object limit. Larger BLAKE3 blobs need Nu transfer, or the verified HTTP mapping once
  it exists ([D14](data.md#framing-and-limits)). The gateway may still *serve* large held
  blobs over RASL to ordinary clients. Fetching refuses loopback, private and link-local
  targets (checked again after redirects) unless configured, and bounds sizes.
- `nu share` emits a `rasl://` link with the public seed as hint when an HTTPS seed serves the
  object. Only SHA-256 objects are browser-verifiable without a Nu client.

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
atproto profile must match interop vectors byte-for-byte. Nu uses indigo's atproto packages
where they fit and cross-checks them against its own core in tests.

### Walk profiles

Pins, CAR export, `/nu/tree/1` and completeness marks share explicit walk profiles:

| Profile | Follows | Complete when |
|---|---|---|
| `nu-tree` | tree header → directory MST → entry metadata, file nodes → blobs | [D3](data.md#d3-directory-trees) |
| `atrepo` | commit → MST → records; blob refs only with `blobs` | all structure and records present; blobs if selected |
| `drisl` | every tag-42 link, within bounds | every reachable block present |
| `direct` | nothing | the object itself is present |

Links out of an `atrepo` walk (other repos' records, unselected blobs) are expected to
dangle. They are reported and do not make the pin incomplete.

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
- The MASL header allowlist, CSP-merge rules and `.tile` rendering headers. These are gated
  on the [API/browser security contract](operations.md#api-gateway-and-public-exposure).
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
