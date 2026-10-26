# IPFS adoption and migration

[Spec overview](../spec.md) · [Product brief](product.md) · [Data](data.md) · [Operations](operations.md)

Nu preserves the IPFS-style workflow and selected libp2p wire protocols. It is initially
neither a Kubo RPC replacement nor a UnixFS/Bitswap content peer. Old IPFS gateways cannot
be assumed to retrieve a Nu CID. The planned bridge is **verified, one-way import** into
Nu, not a second native storage/replication model.

Migration follows the first useful previews. It must be available and demonstrated before
recommending that existing IPFS deployments move their production content to Nu. Reverse
migration/export to UnixFS, ongoing bidirectional synchronization and Kubo API emulation
are not initial migration promises. The standard Linux/macOS production distribution
includes the verified importer; minimal embedded builds may omit it and must advertise that
limitation. Optional build tags do not justify omitting the supported migration release.

## Two journeys

### Import existing content

Given an IPFS root, obtain original blocks from a local installation or the old network,
verify them under their original CIDs, traverse the supported UnixFS/DAG representation,
and materialize the file bytes/tree in Nu. A remote gateway is a transport source, not
verification authority. Verify original links/blocks before deriving new content.

Nu's whole-file BLAKE3/tree representation generally produces different roots. Record the
mapping from original IPFS root to resulting Nu root/version. Do not alias old identifiers
as if the underlying representations were identical.

### Move an installation

Inventory local Kubo pins and IPFS Cluster datasets, policies, labels and automation.
Estimate space, import recoverable content locally without unnecessary network transfer,
map policies with explicit unsupported cases, validate retrieval and Nu replica readiness,
then produce a cutover report. Existing scripts require an API/CLI transition, not a claim
of drop-in compatibility. Never remove or modify the source installation automatically.

## Import contract

- Dry run identifies supported object types, missing content, intended metadata handling,
  estimated disk/headroom and policy mappings. Inventory can be incomplete; say so.
- Interrupted imports resume from verified progress. Bound resources and report errors per
  root/dataset; one unsupported item need not discard completed imports.
- Preserve exact file bytes and representable names/metadata. Define symlink, permissions,
  filename normalization and unsupported DAG/UnixFS forms before shipping. Transformations
  require explicit reporting/choice, not silent loss.
- Report original root, new root/version, verifier outcome, importer/version, imported
  files/bytes, metadata transformations, missing content and applied policy.
- Store provenance in a content-addressed import manifest (including importer version and
  reconciliation report), referenced by the signed dataset version's `imported` field.
  Standalone imports return its CID; exports include it. Conversion does not preserve
  original CID identity; original-block verification and the conversion report establish the
  relationship. A signed dataset version authenticates the manifest reference; a standalone
  manifest CID alone offers content integrity, not authenticated authorship. Record it in a
  dataset to add that provenance. Canonical schema/validation remains design work, not
  another transfer protocol.
- Validation reads from a fresh Nu client, including selected historical versions when
  applicable, and waits for the configured replication policy before suggesting cutover.
- Source credentials/private keys are not copied as incidental metadata. Cluster authority
  and public identity migration require separate deliberate setup.

## Import profile (normative v1)

The bridge is an importer, not an IPFS node. It never serves Bitswap, writes to Kubo, or
claims that a Nu CID is an IPFS CID. Inputs are a canonical CIDv0 or CIDv1 root plus a
source. CIDv0 is accepted as a dag-pb SHA-256 multihash and normalized internally; CIDv1
accepts raw or dag-pb with SHA-256. Identity/inline CIDs, unsupported hash functions and
codecs other than raw and dag-pb are refused (there is no block to retrieve or verify for an
identity CID). Every received block, including each raw leaf and dag-pb node, is hashed using
the CID's own multihash and rejected before parsing/consumption on mismatch. The source is
never trusted to establish identity.

Supported UnixFS profile:

- UnixFS `File` and `Directory`, dag-pb links in encoded order, raw leaves, and recursively
  linked dag-pb file chunks. Concatenate file chunks in link order; verify declared blocksize
  and filesize fields when present against decoded children and bytes. A dag-pb node without
  a recognized UnixFS Data message is unsupported. Unknown UnixFS type, malformed protobuf,
  inconsistent sizes, cycles and excessive depth/link count are errors, not lossy conversion.
  Well-formed fields of the UnixFS Data message outside the schema (numbers above 8, at most
  16 per node), such as fil.pub's authorship signature in field 1001, are skipped; on an
  entry node (root, directory, file or symlink) each is reported as the transformation
  `unknown UnixFS field <n> dropped`. Unknown dag-pb (PBNode/PBLink) fields stay errors.
- `HAMTShard` directories are traversed by links, not materialized as user-visible shard
  directories. Validate fanout/hash type/prefix and link names; reconstruct the logical names
  and reject duplicate or malformed names. Directory and file links with unsupported UnixFS
  types fail that root.
- UnixFS symlinks become Nu D3 `kind: "symlink"` entries whose raw blob contains the
  exact target bytes. Never follow a symlink. Extraction continues to apply operations X1–X6:
  `nu get -o` reports symlinks skipped and never creates them. Special files and metadata
  other than representable executable mode are reported as unsupported transformations.
  Import names byte-exactly, subject to the Nu
  D3 name rules; never normalize/rename silently. Permission bits reduce to Nu's executable
  bit. No timestamps, ownership, xattrs or MIME guesses are preserved.
- Empty files/directories and files with raw or dag-pb leaves are supported. A file entry
  becomes a Nu raw blob (BLAKE3 by default); a standalone file root is wrapped in the normal
  D3a MASL file node. A directory root becomes the D3 Nu MST tree. A standalone symlink root
  has no filename to represent as a D3 entry and is refused with an instruction to import its
  containing directory. Thus all resulting roots/blobs are newly computed Nu CIDs. Existing
  Nu D3 limits still apply, including 8 MiB for whole-object SHA-256 verification; IPFS source
  blocks are individually CID-verified under their original hash and do not become Nu SHA-256
  blobs by implication.

## Sources and transport

`bridge.Source` is a read-only block source by CID. `car import`-style local CAR input and a
local Kubo pinset adapter feed the same verifier/converter as network retrieval. A CAR parser
must reject malformed framing and validate each block CID; it may contain unrelated blocks,
which are ignored and reported. Local Kubo v1 is supported through an operator-generated
CAR (`ipfs dag export <root> > root.car`) and pin inventory (`ipfs pin ls --type recursive
--enc`) rather than opening flatfs/badger internals: those formats are version-specific,
may require locking, and do not provide a stable read-only contract. Inventory commands are
read-only; Nu never edits, unpins, or locks/mutates the source repo. Importing a Kubo repo
path directly is not promised in v1. Cluster inventory/policy import is report-only and
requires explicit operator mapping; no IPFS Cluster API or authority/key migration is
implied.

Remote blocks use Nu's own QUIC-v1 host and a minimal client-only Bitswap implementation;
provider lookup uses the existing Amino DHT (`kad/`). Request only the root and referenced
blocks, enforce bounded concurrency, response/block byte limits, deadlines and traversal
budgets, and validate every CID before accepting a block. Bitswap's roughly 2 MiB block-size
limit is enforced; unsupported/larger individual blocks fail explicitly. Each block is wanted
(`/ipfs/bitswap/1.2.0`, `wantType: Block`, `sendDontHave`) from up to 8 providers at once,
10 s per dial and want, 60 s per block; the first block that verifies wins. A provider's
`DONT_HAVE` presence counts as a refusal, and a block every reachable provider refused or no
provider could be reached for is *missing* (not corrupt). CIDv0 blocks arrive with the
prefix `[0, dag-pb, sha2-256, 32]`. No go-libp2p or
boxo dependency is permitted in the main module.

HTTP trustless gateways are the second network transport. Filecoin storage providers
(Curio, as used by Filecoin Onchain Cloud) serve IPFS content only this way and announce it
to IPNI with the `transport-ipfs-gateway-http` protocol, without Bitswap. For a block the
importer asks the delegated-routing endpoint `ipfs.routing_url` (default
`https://cid.contact`; `off` disables the transport) for
`GET /routing/v1/providers/<cid>` (JSON or NDJSON, at most 256 KiB and 64 records, 15 s),
keeps `peer` records listing `transport-ipfs-gateway-http`, and turns their
`/dns|dns4|dns6|ip4|ip6/<host>/tcp/<port>/https` (or `/tls/http`, `/tls/sni/<h>/http`,
`/http`) addresses into at most 8 gateway base URLs. It fetches
`GET <gateway>/ipfs/<CIDv1>?format=raw` through `httpsrc` (the URL-import SSRF policy and
`ingest.allow_http`/`allow_ips`; 2 MiB per block, as for Bitswap; 10 s idle, 60 s per
request), trying gateways one at a time, and verifies the bytes against the CID. A gateway
that serves wrong bytes is demoted and the next is tried. Gateways that already served a
block are asked first for later blocks, so a DAG held by one provider needs one routing
lookup. Gateways answer one block per request, so the transport reads ahead in walk order:
the children of each arriving dag-pb node are queued before earlier-queued blocks, and up to
8 queued blocks are fetched at once while fewer than 32 fetched blocks wait to be asked for
(no read-ahead for dry runs, which skip raw leaves). A 404 from the routing endpoint means
no providers; a block no gateway supplied is *missing*. Bitswap and the gateway transport are raced for each block until one has served:
the first verified block wins and its transport is asked alone for the next block, falling
back to the race when it fails. Either transport alone suffices. A private or isolated
cluster member never uses the gateway transport (D15). The result's `source` is `car`, or
the network transports used: `bitswap+gateway`, `bitswap` or `gateway`. Gateways never
replace original-CID verification.

## Resumption, provenance and cutover

An import is a durable operation with a stable ID, root, source description, stage and byte /
file progress. Persist each successfully CID-verified source block and completed conversion
checkpoint before advancing; on restart, revalidate retained blocks and resume from missing
links. Never treat an unverified partial or source-reported progress as completion. A repeated
root import may reuse verified blocks and already materialized Nu objects. Cancellation keeps
verified checkpoints and leaves source untouched. Failed/unsupported roots retain per-root
errors; no incomplete root is advertised as complete or pinned as the requested result.

Write a canonical content-addressed DRISL provenance manifest containing schema/version,
original IPFS root (canonical CID), resulting Nu root, importer build/version, verification
outcome, source kind, completion time, imported file/byte counts, per-file relative path and
original entry CID(s) → resulting blob CID, transformations/skips, and missing/unsupported
items. `metadata_transformations` lists `{path, cid?, reason}` items: the standing note that
only the executable bit is kept, then per node a dropped mtime, a file mode other than
0644/0755 reduced to the executable bit, a dropped directory/symlink mode, and each symlink
kept as a D3 symlink entry that extraction skips. Do not store credentials or local secrets. Validate the manifest's canonical schema
and hash it as Nu DRISL; return its CID. A manifest CID proves integrity only. For signed
atproto/cluster dataset history, reference it from the version's `imported` field, whose
signature authenticates that reference. Standalone import makes no authorship claim. An import
with a dataset (`--dataset <ds>`, an archive dataset: the reference lives in its version
record) writes the Nu root as the dataset's head and, in the same signed commit, a version
with `imported: {source: "ipfs://<canonical root CID>", manifest: "<manifest CID>"}` (part of
the version's identity, docs/cluster.md). A replay of the same import finds that head and
reference and writes nothing. Datasets without history and pinsets are refused with
`--dataset`: one version names one root.

Dry run reports root/profile, inventory completeness, supported and unsupported items,
missing blocks, estimated logical/physical bytes and headroom (estimates clearly marked),
metadata transformations, and proposed policy mapping. A dry run is a **structure-only
walk**: it retrieves and verifies under the original CIDs the root, every directory and HAMT
shard, every symlink and the top node of every file, but no file chunk or raw leaf. File
sizes are therefore declared ones (UnixFS `filesize`, or `Tsize` for a raw-leaf link), and the
physical estimate adds Bao outboards per file; it excludes database/DRISL overhead and may
overcount shared blobs. The walk continues past missing blocks (`complete: false`, counts
cover what was reached) and unsupported items (unsafe or duplicate names, unknown UnixFS
types, malformed nodes, a standalone symlink root), listing each with its path. A CAR, being
local, is additionally converted in memory to report the planned Nu root and manifest; a
network dry run plans no root. Neither writes to the store or checkpoints blocks. Do not infer
readiness from import:
validate by reading the new root through a fresh Nu client, then wait for the exact configured
replication policy and target before recommending cutover. Report a reconciliation between
source pins/inventory and imported roots, missing/unsupported content and policy differences.
Operators test scripts against Nu APIs/CLI, validate fresh-client retrieval and readiness,
then redirect writers/readers deliberately. Back up Kubo and keys separately. Cutover never
automatically stops Kubo, removes pins, deletes blocks or transfers identity/cluster authority.

**Pinset import.** A Kubo repo is inventoried read-only with `ipfs pin ls --type recursive
--enc=json [--names] > pins.json` (the `{"Keys": {cid: {Type, Name}}}` object, or the
`--stream` form of one `{Cid, Type, Name}` object per line; at most 64 MiB). The inventory is
parsed when the operation starts and kept in its job, sorted by CID. Each recursive pin is one
root: from `<car-dir>/<cid>.car` (the CID string exactly as listed; export each with `ipfs dag
export <cid> > <car-dir>/<cid>.car`) or, without a CAR directory, over Bitswap. Roots are
processed in order and each gets one report entry, persisted before the next starts, so a
resumed job continues with the next root: `imported` (Nu root, manifest, files, bytes,
transformations), `planned` (dry run: complete and supported), `missing` (no CAR, or a block
no source supplied, named), `unsupported` (outside the import profile, or an invalid CID),
`failed` (a corrupt CAR/block or another source error) and `skipped` (not a recursive pin:
direct and indirect pins do not name complete DAGs to import). One root's outcome never
discards another's; the operation succeeds with the reconciliation report, whose counts per
status and totals (and, for a dry run, the blob estimate against headroom) close it. Nu never
opens the Kubo repo itself.

**Cutover check.** `nu import cutover <ds> [--cluster] [--timeout 10m]` (`POST
/import/ipfs/cutover`) evaluates three gates for the dataset's current head and recommends
cutover only when all pass; otherwise it lists what is missing:

1. the head version carries an `imported` reference whose manifest is held, is an
   `io.ipfs.nu.import` record, is `verified`, and maps that source to that head;
2. a **fresh client** — a new node with a new key and an empty temporary store — retrieves
   the head completely over QUIC from this node and the connected cluster members; it never
   reads the daemon's store directly. In private and isolated clusters the connection gate
   refuses non-members, so this gate cannot pass there (validate with `get` from a new member);
3. the exact dataset policy is `ready` for that head (the W-rule wait, bounded by
   `--timeout`).

The report repeats the standing rules (Nu changes nothing in Kubo; back up the Kubo repo and
keys; move readers/writers deliberately) and notes a single-copy policy.

CLI/API: `nu import ipfs <cid>` retrieves through Bitswap/Amino and HTTP gateways; `nu import ipfs --car file
<cid>` imports a local CARv1; `nu import ipfs --pins pins.json [--car-dir d]` imports a
pinset. `--dry-run` applies to all three. The local API's `POST /import/ipfs` accepts the same
request and returns a durable operation ID. Operation request/stage/result survive daemon
restarts. During conversion each original source block is verified and atomically
checkpointed under `imports/<operation>/blocks` before it can be used; resumed CAR and network
jobs revalidate and reuse these checkpoints, so verified blocks are not requested from the
source again (a CAR is still re-indexed into a temporary file on restart). A daemon shutdown
leaves a running job's descriptor running, and the next start resumes it under the same
operation ID; an explicit cancellation records the cancellation and the job is not resumed.
When an operation succeeds with every root imported, its checkpoints are deleted; otherwise
they are kept with the job. Production Linux/macOS builds include the bridge; minimal builds
expose an explicit unavailable-feature result.

## Acceptance

Test Kubo-produced files, directories, HAMT-sharded directories, raw leaves, CIDv0 and large
files; compare extraction byte-for-byte with `ipfs get`. Reject modified/missing blocks,
malformed dag-pb/UnixFS, unsafe/colliding names and invalid HAMT data; assert symlink target
bytes round-trip into D3 and extraction reports them skipped under X3.
Test interrupted resume, source non-mutation, CAR and Kubo pin inventory, and a Bitswap fetch
from Kubo over QUIC-v1 with Amino provider discovery. Keep small Kubo-generated fixtures
vendored for fast offline tests and fuzz seeds for dag-pb/UnixFS decoders. Docker/Kubo fixture
generation and live interop are opt-in (`NU_DOCKER=1`). A clean Nu client must retrieve
imported bytes; cutover report must distinguish unsupported/missing items and exact-policy
readiness. Until those gates pass, migration is not a supported production recommendation.
