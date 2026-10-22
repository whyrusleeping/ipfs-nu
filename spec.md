# Nu

**Draft design.** Nu is a Go daemon and CLI that brings back the magic of IPFS: add files,
share a link, fetch them elsewhere and open them in a browser. Versioning, verified storage
and managed replication are built into the same product so individuals and institutions do
not need a second stack once their data matters. It uses atproto identity and signed records
for public mutable names and implements a focused, wire-compatible libp2p data network.

```sh
nu add -r ./my-site
nu share <tree-cid>
# On another machine:
nu get <share-link>
# Publish a stable name when wanted:
nu publish site <tree-cid>
nu get alice.example.com/site/index.html
```

## Specification map

| Document | Contents |
|---|---|
| [Product brief](docs/product.md) | Audience, magic + reliability, progressive journeys and maintainability boundaries |
| [Operational contracts](docs/operations.md) | Readiness, served versions, retention, capacity, security, recovery and failure acceptance |
| [Local API and CLI](docs/api.md) | `$NU_PATH` layout, error codes and exit categories, paging, operations, v0 routes, loopback protections, targets |
| [Data, transfer and storage](docs/data.md) | CIDs, blobs, directory MSTs, file nodes, sync, discovery, pins and GC (D1–D3a, D14–D15, D17) |
| [DASL and atproto data](docs/dasl.md) | DASL/BDASL conformance, DRISL/CAR/RASL/MASL/FISL, atproto repo profile, repo import/export/browse and archive datasets (D21) |
| [Identity and names](docs/names.md) | Accounts, current public pointers, records, proofs, caching and updates (D4–D7) |
| [Networking](docs/networking.md) | Wire profiles, sessions, relays, NAT traversal, discovery and testing (D8–D13, D16) |
| [Following datasets](docs/follow.md) | Current mirrors, delta sync, update notification, swarms, scopes and budgets (D19) |
| [Clusters](docs/cluster.md) | Integrated versioned archives, signed state, placement, repair, history, network modes and bundles (D20) |
| [Collections](docs/collections.md) | Growing item collections: metadata facets, member-selected and rolling holdings, rescue, union heads, item push API, holdings index (D22) |
| [IPFS migration](docs/migration.md) | Installation migration and verified one-way content import, provenance and cutover |
| [Validation and measurements](docs/validation.md) | Experiments, pilot workloads, measurement method and release evidence |
| [Pilot workloads and gates](docs/pilots.md) | Workload matrix, pilot intake and result templates, benchmark fixtures/runner, sub-file reuse and capacity-UX decision gates (templates awaiting real pilots) |

This entry point owns goals, local interfaces, implementation structure, milestones and open
decisions. Detailed requirements live in the owning documents; decision IDs remain for
cross-references. Design history is not retained.

## Goals and scope

Two first-class workflows share one data model:

1. **Share:** one binary, familiar add/get/cat/pin commands, links and browser access. No
   account for content commands, no manual network setup on supported public networks,
   offline LAN use, and explanations rather than mysterious failure. Ease and fun are release
   criteria, not polish deferred until after protocol work.
2. **Keep:** versioned datasets with deliberate retention, verified retrieval, integrated
   replication and repair, truthful readiness and tested restore. Initial team users are
   smaller Internet Archive/Wikipedia-shaped archives and mirrors; individuals get a simple
   one-node archive path without a separate service stack.

Supporting commitments:

- **Connectivity:** dial PeerIds or DIDs, relay when necessary, upgrade to direct paths,
  survive network changes. Initial QUIC previews exclude UDP-blocked paths; production
  public reach adds HTTPS-compatible access/relay paths with declared endpoint requirements.
- **libp2p wire compatibility:** interoperate with deployed go-, rust- and js-libp2p for the
  [selected profile](docs/networking.md#d8-wire-profile-and-compatibility). Transport/routing
  compatibility does not imply IPFS content or API compatibility.
- **Names:** existing atproto accounts supply public current-state pointers, accepting stale
  cache answers with visible freshness. Names are not archives; archived versions survive
  independently under dataset retention.
- **Verified transfers:** whole-file BLAKE3, verified ranges, resumable multi-peer retrieval.
  Structural diffs do not imply sub-file delta transfer.
- **DASL and atproto data:** Nu's data model is DASL. Structure is SHA-256 DRISL, large
  blobs are BDASL, and any DASL/BDASL object can be added, pinned, moved, served (including
  RASL) and exported as CAR. atproto repositories are a native structure: verified record
  fetch, repo CAR import/export/browse, and accounts as versioned archive sources
  ([D21](docs/dasl.md)).
- **Maintainability:** one daemon, common store/pin/GC system, shared byte/tree protocols and
  versioned API; small reusable packages, not a new sidecar or protocol family for each job.

**Initial platform constraints:** Go, Linux amd64/arm64 servers and Linux/macOS amd64/arm64
workstations, no cgo, MIT license, domain `nu.ipfs.io`. Linux builds are static; macOS
packaging/signing and native behavior are tested separately. Implement our own libp2p subset
over stock quic-go; do not depend on go-libp2p, rust-libp2p Swarm, iroh or iroh-blobs. Iroh is
behavioral inspiration, not the wire target. macOS storage-member support beyond the tested
workstation/one-node path must be qualified by release evidence.

**Not initial promises:** running atproto infrastructure; arbitrary libp2p features; Kubo RPC
compatibility; native UnixFS/Bitswap exchange; incentives, anonymity, encrypted private
sharing or per-dataset read permissions. Trusted-member clusters with OS/disk protections
are sufficient initially. Private/isolated members need direct routed/VPN connectivity;
membership is access control, not content encryption. Nu is not an Ethereum consensus node,
POSIX filesystem, live database or automatically a complete backup service.

Verified one-way IPFS import and installation migration follow the first useful previews;
they gate recommendations to migrate existing installations. Browser-native transports,
other operating systems, encryption/Spaces and self-hosted public names come later. Current
names have no history; dataset archives have explicit history/availability semantics.

## User experience

```text
nu init                              # optional: any command initializes $NU_PATH on first use
nu daemon                            # libp2p UDP :4171, local API :5171, gateway :8171
nu status                            # one summary: daemon, reachability, shares, datasets, cluster
nu doctor                            # plain-language reachability and recovery actions

nu add photo.jpg                     # MASL file node with metadata → BLAKE3 raw blob; prints CID + share link
nu add --raw photo.jpg               # whole-file blob CID
nu add --hash sha256 photo.jpg       # core-DASL blob ≤ 8 MiB (browser/atproto-verifiable, no Bao)
nu add -r ./dir                      # tree CID; unchanged files not rehashed
nu tree edit <root> --put p=<cid> --rm p
nu add --no-announce photo.jpg       # unannounced, not private
nu add --nocopy photo.jpg            # mutable local source, not an independent archive copy
nu add https://example.org/f.tif     # URL import: hashed on arrival, provenance recorded
nu cat <cid>
nu get <cid-or-link> [-o path]
nu ls <tree-cid>[/sub/path]
nu pin add|rm|ls <cid>
nu share <cid>                       # short link with peer hints; HTTPS/rasl:// link when a public seed serves it
nu public add|rm|ls <cid>            # opt a held root into this node's public HTTPS seed ([public] config)

nu dag get <cid>[/path] | dag put    # DRISL ↔ atproto-style JSON ($link/$bytes)
nu car import|export <cid|file> [--profile nu-tree|atrepo|drisl]
nu get rasl://<cid>/?hint=host       # verified HTTPS retrieval (DASL RASL)
nu get at://<did>/<collection>/<rkey> # verified atproto record → JSON
nu get at://<did>                    # verified repo snapshot; ls/cat browse it
nu masl bundle <tree-cid>            # export a MASL bundle / .tile CAR

nu login alice.example.com           # atproto OAuth, Nu records only; nu logout
nu node register [--label laptop] [--seed]
nu publish <name> <cid> [--wait-seed <peer>]  # public name write, NOT replication readiness; the seed wait is
nu resolve alice.example.com/<name> [--fresh|--offline|--max-age 1h]   # a separate result (W4)
nu get alice.example.com/<name>[/path]
nu names | unpublish <name>
nu follow alice.example.com/<name> [--cluster --all] [--max-size n] [--no-announce]  # --path/--lazy, DID-wide: later
nu follow ls | refresh | nu unfollow <ds-or-name>   # a follow is a mirror dataset: observed/syncing/held, lag, blockers
nu devices

nu dataset create <ds> --archive|--mirror [--source …]  # one-node datasets: no cluster setup, no PDS
nu dataset add|edit|update|status|versions|diff|label|rollback <ds>  # sources: local, Nu name, atproto account

nu cluster init|invite|join|status   # multi-machine: same dataset verbs as `nu cluster dataset …`
nu cluster dataset create|add|edit|update|status|versions|diff|label|rollback <ds>
nu cluster collection items|rules|where <ds>        # item metadata, local selection, copies per item
nu cluster collection put <ds> <path> --meta m.json … # producer pushes an item (upload, path or URL)
nu repo export|import <dir>          # verified content bundle of every pin (stopped node)
nu repo backup|restore <file>        # keys + signed cluster state, separate from content

nu relay                            # circuit relay + dnsaddr/peer-routing server
```

The owning contracts require mirror/archive presets, exact-version waits, destructive dry
runs and machine-readable results; precise new flag/API syntax is implementation design
work, not settled by this inventory. First share must not require cluster administration.
See [product journeys](docs/product.md#progressive-experience) and [links](docs/names.md#links).

**First run.** Any command initializes `$NU_PATH` (default `~/.nu`) if absent. When no daemon
is running, `get`, `cat`, `ls` and `share` run an in-process node for the duration of the
command, so a recipient can install the binary and immediately `nu get <link>`. A
foreground `nu share` keeps serving until interrupted and says so; the shared content is
available only while some holder is online.

**Targets.** `get`, `cat` and `ls` parse their argument in this order:

| Form | Meaning |
|---|---|
| `nu://…`, `at://…`, `rasl://…` | Link, atproto record/repo, RASL fetch |
| `b…` CID, then optional `/path` | Immutable content |
| `<handle-or-did>/<name>[/path]` | Public name; the first segment contains a `.` or starts with `did:` |
| `<dataset>[@<version>][/path]` | This daemon's dataset; dataset names match `[a-z0-9-]+`, so never contain `.` |

Anything else is an error that lists these forms. `nu add` alone accepts local paths and
`https://` URLs.

## Architecture

Two independent planes:

- **Name plane:** HTTPS to atproto identity/PDS services; signed records map public names to
  CIDs and DIDs to node PeerIds. Notifications prompt verified current-state refreshes.
- **Data plane:** libp2p sessions carry verified bytes, trees and cluster-local signed state.
  Fetchers locate providers through hints, publisher nodes, LAN and optional routing services.

The CLI talks to the daemon API, or to an in-process node when no daemon runs. The daemon composes naming, networking, fetching, local
storage, history and replication; the gateway serves verified retrieved content. Cached
names and CID retrieval work during atproto outages. Seeds retain published content; an
exact-target wait, not a publication write, tells a laptop when it may sleep.
Clusters add signed shared state and placement, reuse follow/sync machinery, and operate
without public infrastructure in private/isolated modes. Trusted archival and public
connectivity can therefore develop in parallel rather than one waiting for the other.

## Local API, gateway and browsers

- API: `127.0.0.1:5171`, versioned JSON HTTP/OpenAPI; non-loopback requires authentication.
  No Kubo API compatibility. Stable errors, exit categories and structured status.
- API conventions, shared by every endpoint and CLI command: one error envelope
  `{error: {code, message, action?, details?}}` with stable codes mapped to CLI exit
  categories; `--json` on every command; list endpoints page with opaque cursors (`?since=`,
  `?cursor=`) and answer `resync` rather than an empty delta when a cursor has expired;
  long-running work (fetch, follow sync, add/import, item push, migration, bundles) is an
  **operation** resource with an ID, exact target, stage, progress, NDJSON event stream and
  `wait` with timeout. The CLI shows TTY progress from the same events and is built only on
  the public Go `client` package. Codes, routes and layout: [docs/api.md](docs/api.md).
- Gateway: loopback by default, port `8171`; `/nu/<handle>/<name>/<path>`, `/tree/<cid>/…`,
  `/blob/<cid>`, `/c/<dataset>[@<version>]/<path>`, `/at/<did>[@<rev>]/…` (held atproto
  snapshots, inert rendering) and `/.well-known/rasl/<cid>`. Active untrusted content
  (including MASL bundles) needs isolated origins or a safe non-active fallback, not
  unrestricted same-origin path hosting. The planned local mechanism is one origin per
  root, `http://<cid>.localhost:8171/` (a 36-byte CID is 59 base32 characters, within a
  DNS label); the [security contract](docs/operations.md#api-gateway-and-public-exposure)
  must verify it in each browser, including Safari, before path routes render active content.
- Local layout: `$NU_PATH` (default `~/.nu`) holds `config.toml`, the node key, the SQLite
  database and the blob directory. Config keys are dotted TOML paths (`gc.release_delay`);
  `nu config get|set` edits them. Linux servers use `NU_PATH=/var/lib/nu`.
- Public seed/gateway: explicit HTTPS serving, listings/downloads and `Range` early enough
  for browser magic. Convenience browser reads trust gateway/TLS; Nu-aware clients verify
  hashes/proofs. Verified HTTP range/proof mapping is separately specified before claiming it.
- HTTPS-compatible public relay fallback ships before broad public recommendation.
  WebTransport/WebRTC and a future Nu WASM client are later extensions; stock Helia content
  interoperability is not promised without a Nu protocol/client adapter.

The [API/browser security contract](docs/operations.md#api-gateway-and-public-exposure) is a
feature entry gate, not a post-release hardening task. Members-only libp2p gating does not
protect an exposed HTTP gateway.

## Sharing and privacy

| Mode | Behavior |
|---|---|
| Published | Public atproto record, visible on the firehose; not an availability promise |
| Announced | Public-mode `nu add` provides by default; CID lookup can discover providers |
| Unlisted | `--no-announce` + share hints; not access control or concealment from contacted peers |
| Cluster (private/isolated) | Trusted members only, no public providing/relays; directly reachable storage members |
| Private (later) | Content encryption; key holders decrypt |
| Private names (later) | atproto Spaces |

Node records contain relay/dnsaddr hints, not raw IPs; public signed peer records/DNS can
still expose direct IPs unless relay-only. Relays observe peers/traffic volume, not inner
content; DHT lookups expose interest. Explain exposure in the chosen mode without adding
repeated confirmations to ordinary sharing. Publication never silently changes cluster
access policy.

## D18. Implementation layout

Module `github.com/whyrusleeping/nu`. Packages are ordinary importable packages; the project
does not use Go `internal/` directories. Wire and store formats, not Go APIs, are the stable
contracts until a package is declared stable; the HTTP API and `client` are the supported
programmatic surface.

```text
cmd/nu/                 CLI + daemon
core/                   CID (DASL/BDASL), DRISL, profiled MST (nu-dir, atproto v3), CAR,
                        MASL, atproto commit/proof/diff, walk profiles, tree/proofs,
                        BLAKE3/Bao; no I/O
p2p/wire/               keys/PeerId, multiaddr, multistream-select, TLS extension, protobuf
p2p/magicsock/          PacketConn, virtual addresses, relay/direct paths, disco, OOB/GSO
p2p/host/               quic.Transport, sessions, dial race, connection management, limits
p2p/proto/              identify, ping, AutoNAT v2, circuit v2, mDNS, nu/qpath, nu/path
p2p/portmap/            UPnP-IGD, PCP, NAT-PMP
p2p/netmon/             interface/route monitoring
p2p/lookup/             cache, hints, dnsaddr, routing/v1, DHT integration
kad/                    Amino-compatible Kademlia
proto/                  nu/bytes, nu/tree, nu/have, nu/name
follow/                 update state, notification sources, delta sync, swarm
cluster/                state repos, membership, placement, history, handoff, repair,
                        audit, status, bundles; proto /nu/cluster/1
collection/             facets, selection rules, union heads, holdings log and index,
                        item push API
store/ names/ fetch/ node/ api/ gateway/ client/
nutest/                 in-process multi-node test harness (temp stores, loopback QUIC,
                        partitions, restarts, clock skew)
atproto/                record fetch, repo snapshot/diff sync, atrepo dataset source (indigo)
httpsrc/                RASL/FISL HTTPS retrieval and URL import with SSRF guards, shared by
                        fetch/names/ingest
rasl/                   rasl:// URLs and the hinted-host HTTPS source (fetch.Source) on httpsrc
relay/                  circuit HOP + dnsaddr/routing server
bridge/                 later IPFS import: Bitswap, UnixFS; enabled in standard Linux/macOS
                        production distribution, optional/build-tagged for minimal embedders
bench/                  seeded workload fixtures (fixtures/, cmd/fixturegen) and the opt-in pilot
                        benchmark runner (pilot/); docs/pilots.md
dist/ scripts/           service definitions (systemd, launchd), install/upgrade and release
                        scripts
experiments/            separate Go module for spikes; may use go-libp2p/Kubo, never imported
```

Dependencies: stock `quic-go`, `indigo`, `lukechampine.com/blake3`, `go-cid`/`go-multiaddr`
leaf modules, `github.com/decred/dcrd/dcrec/secp256k1/v4` (libp2p secp256k1 verification;
stdlib does not implement that curve), `modernc.org/sqlite` (metadata, DRISL blocks, pins, indexes; blobs are files),
`miekg/dns`, `BurntSushi/toml`, Prometheus client. Adapt Tailscale's
BSD-3 portmapper/netmon instead of importing `tailscale.com`. WSS/Noise/yamux, when needed
for reach, belong to networking rather than an import-only build tag. The TCP relay profile
uses Nu-owned bounded WebSocket and yamux implementations, stdlib `crypto/ecdh` X25519 and
SHA-256, and the existing `golang.org/x/crypto/chacha20poly1305` for libp2p Noise XX; no
additional module is selected. Retain stock QUIC/TLS; own security-critical code requires
parser fuzzing, interop, NAT lab and independent review.

## Milestones

Implementation gates, not achieved-performance claims. A preview limitation is not a v1
promise. [Validation](docs/validation.md) owns reproducible measurements and pilot limits.
Existing backlog `0a/0b/0b+/0c` prefixes name technical workstreams, not a serial product
release order. Sharing and keeping must reuse foundations but have independent thin slices.

**v0 is the first target and comes before everything else.** It means add, share, get and
browse between two machines that can reach each other directly (same LAN, routed, or public
addresses): the core formats and store, direct QUIC sessions with mDNS and hints,
`/nu/bytes/1`, a multi-peer fetcher, and the local CLI/API/loopback gateway, including a
recipient with no daemon. It leaves out relays, NAT traversal, DHT, names, clusters, history,
public HTTPS and atproto. v0 tasks carry a reduced "v0 cut" of evidence. Fuzzing campaigns,
cross-implementation checks, benchmarks and scale tests follow in a hardening task, which
gates the gate-1 and gate-2a exits below but not v0.

| Gate | Scope | Exit criteria |
|---|---|---|
| **v0 — First runnable share** | Backlog p1 tasks: 0001, 0002, 0004–0010, 0022, 0027, 0028, 0040, 0060, 0065, 0066 | Two machines on one LAN or routed network: `nu add -r`, share link, `nu get` (also with no daemon running) and browsing in the local gateway; the fast test suite is green |
| **0 — Foundations and risk retirement** | Scaffold, Linux/macOS, canonical DASL formats/store and atproto repo core, local walking skeleton (CLI/API/gateway on the core store, in-process multi-node harness), libp2p wire, security contracts, Q2/Nu-path spikes, pilot selection | Fuzz/vector/interop evidence, including DASL test suite and atproto interop vectors; bounded discovery/path experiments; defined browser and readiness mechanisms before dependent features ship |
| **1 — First magic preview** | Hint/LAN/direct + bounded development relay retrieval, bytes/trees, resumable fetch, CLI/local gateway (incl. RASL, `dag`/`car`), explicitly operated preview HTTPS seed | Fresh users add/share/get/browse without an account on declared networks; two laptops share 10 GB/10k-file tree; 3-peer verified download and restart; HTTPS via a supplied preview seed or operator's domain/TLS, not automatic project hosting; no DHT/hard-NAT prerequisite |
| **2a — CollGS pilot preview** | Thin private-mode slice parallel with 1, for the [CollGS pilot](docs/validation.md#pilot-candidates): direct/mDNS sessions, signed membership/dataset/writer-head state, collections with `replicas: 0` (selection, rescue, union heads, holdings index), item push API and URL import, versioned index/push JSON API. Excludes placement, readiness evidence, history, public networking, names and RASL | CollGS pilot criteria on a multi-host private deployment with vmx's producer and dashboard; published preview limits (trusted members, no guaranteed copies, measured throughput/catalog/index figures) |
| **2 — Real users pilot** | Trusted archive track parallel with 1: one-node mirror/archive presets, clusters, basic history, policy readiness, repair/drain, capacity and restore, atproto-account datasets, collection floor placement. Public seed track adds OAuth/names/follow and `at://` record fetch after gate 1 connectivity and naming security | Linux/macOS one-node archive/update/historical read/restore (including APFS crash/sleep/filename tests); 3+ Linux storage members enforce copies/zones and honest unknown/degraded status; repair with surviving data/capacity/authority, crash/disk/partition/key-restore tests. Separately, named publication + exact-seed wait survives laptop sleep |
| **3 — Production v1 and adoption** | Combine magic + archival, names/public mirrors, HTTPS-compatible relay reach, secure HTTPS gateway (RASL, MASL bundles on isolated origins), FISL discovery client, packaging/upgrades/ops/security review, IPFS migration | Linux/macOS first-run UX; UDP-blocked Nu relay path when declared HTTPS endpoint reachable; tested restore/upgrade, published support limits and operated infrastructure; verified resumable pinset/network import before recommending IPFS cutover |
| **4 — Scale and optimization** | D19/D20 scale targets, full retention schedules/span index, large swarms, hard-NAT tuning/datagram/peer relays, air-gap workflows | Measured representative update/storage/repair costs; 10M-entry churn and 1k-follower PDS-load benchmark; PB/100-member sizing validated progressively; complete/incremental bundle tests; published hardware/field NAT pair results |
| **5 — Extensions** | Standard-peer DCUtR, FISL serving/Web Tiles rendering, browser-native transports/client adapters, optional gossipsub, encryption/Spaces, self-hosted single-account names, other OSes | Separate negotiated interoperability/security gates; do not imply stock Helia serves Nu formats or encrypted sharing exists sooner |

Hard-NAT ≥98% EIM↔random-EDM direct remains a conditional lab hypothesis to measure, not
an initial release requirement. Names target cached resolution without name-plane requests
and benchmarked cold p95 <1 s under a declared healthy environment, not a PDS SLA. Extreme
scale and complete air-gap features do not delay first sharing or basic trusted archival.

## Open decisions and verification

- **Amino BLAKE3 keys (Q2):** use native CID multihashes for Amino provider keys, including
  ordinary BLAKE3 blob CIDs; do not derive a SHA-256 routing alias or add a Nu DHT protocol.
  The opt-in live check retrieved random BLAKE3 blob providers from independent Amino clients,
  but the deterministic follow-swarm key timed out without a result. Treat swarm-key DHT
  discovery as unverified/best-effort until that exact key succeeds; hint/local retrieval is
  independent. See [validation](docs/validation.md#q2-amino-provider-keys-and-citizenship-bounded-live-results-incomplete-swarm-run).
- **Public infrastructure:** operators, funding, admission/abuse/quotas, regional deployment
  and domain control before default-service promises; account-free sharing remains a goal.
  A share-link HTTPS gateway (streaming from an online sharer, download-only on a sandbox
  domain, capped, uncached) would give browser recipients the first-share magic without
  hosting obligations; it is a proposal with abuse/cost questions, not a commitment.
- **Name:** the product/binary name `nu` is provisional and clashes with Nushell's `nu`
  executable. Keep the name in few places (`cmd/nu`, one constant, `NU_` env prefix) so a
  rename stays mechanical.
- **Operational mechanisms:** exact-version confirmations, freshness/epoch binding and safe
  release during partitions; retention availability, conflict handling and budget accounting.
  Product contracts do not substitute for these wire/store rules.
- **Security:** browser/API isolation, private gateway readers, OAuth/discovery validation,
  authenticated probe/destination authorization, invite/revocation and root recovery.
- **Pilot performance:** data/change shape, whole-file costs, capacity adjustment UX and
  whether sub-file reuse must move forward: criteria in [pilot decision gates](docs/pilots.md#decision-gates),
  evaluated against real pilot data. Publish measured support limits, not sizing guesses.
- **Format precision:** [data details](docs/data.md#details-still-to-specify),
  [DASL/atproto details](docs/dasl.md#details-still-to-specify),
  [collection details](docs/collections.md#details-still-to-specify), version-tree /
  span-index encoding, canonical placement and tombstone/revocation behavior.
- **Connectivity:** [network verification](docs/networking.md#details-still-to-verify), stock
  path recovery/MTU/congestion, HTTPS-compatible fallback and independent security review.
- **Migration:** implementation and evidence remain open for representative Kubo pinset reconciliation, production cutover/readiness workflows, and live Bitswap-over-QUIC interoperability; the v1 import profile and manifest are specified in [migration.md](docs/migration.md).
- **Lifecycle/portability:** DID-wide following, quotas for future names, macOS metadata
  (xattrs/resource forks are not recorded), signed macOS packaging and native macOS
  crash/sleep evidence; filename handling, export, upgrades and key/state recovery are
  [specified](docs/operations.md#b-backup-export-and-recovery).
- **Deferred:** PDS file serving remains deferred/disfavored; upstream QUIC multipath may be
  reconsidered without requiring a quic-go fork.

Protocol references live with the relevant technical docs. Unresolved mechanisms have
backlog tasks and feature gates; neither new documents nor a green task status constitute
performance evidence.
