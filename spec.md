# Nu — design draft for discussion

> Status: **DRAFT v0.7: design baseline; remaining questions are deferred or verification tasks.** Decisions are numbered (**D1…**) and open questions
> are numbered (**Q1…**) so we can refer to them. Nothing here is final.
> Working name "Nu" (from the repo name `ipfs-nu`).
> Facts about atproto / IPFS / libp2p / DASL / iroh / libraries were checked against the
> sources in §19 (Sept 2026).
>
> **Direction so far:**
> - v0.2: go back to what go-ipfs was for: **a daemon + CLI that makes it trivially easy to
>   share files peer to peer**. atproto provides **identity** (DIDs, handles) and the **signed
>   repo**, used as **a better IPNS**.
> - v0.3: Rust, built on iroh.
> - v0.4: **wire-compatible with the deployed libp2p world** (IPFS/Kubo/Helia, Ethereum
>   clients, rust/go/js/nim libp2p). We implement **a chosen subset of libp2p ourselves, and
>   implement it very well**, with no go-libp2p or rust-libp2p `Swarm`. **iroh is the model for
>   behaviour** (hole punching, relays, address lookup, network changes), not a dependency.
> - v0.5: **QUIC-only for v1** (no TCP transports). **Nu↔Nu connections use
>   magicsock/disco underneath one libp2p QUIC connection**; the extension is negotiated, so
>   other libp2p peers see standard libp2p. **Implementation language: Go** (Q15, confirmed). The
>   default-relay question is deferred.
> - v0.6: domain **nu.ipfs.io**; MIT license; Linux first; BLAKE3 everywhere
>   (IPLD + BDASL); Bao; no Kubo API compatibility; sync protocols designed to replace
>   Bitswap/Graphsync for Nu data (D14).
> - v0.7 (this version): per-directory MST trees, wrapped single files, binary sync framing,
>   maximum-effort hole punching (D11c), all leans accepted. Top requirement: **things just
>   work.** Changes are listed in §16.

## 1. Pitch

```
$ nu add -r ./my-site
added bafyrei…xq  my-site  (212 files, 48 MB)

$ nu publish site bafyrei…xq
published at://alice.example.com/nu.name/site → bafyrei…xq  (rev 3m2k…)

# on any other machine, on any network
$ nu get alice.example.com/site/index.html
```

- **A libp2p node that just works.** It speaks standard libp2p (QUIC, identify, Kademlia,
  circuit relay v2, AutoNAT v2, mDNS), so it connects to Kubo, Helia, Ethereum clients and
  other libp2p nodes. It *behaves* like iroh:
  - dial peers, not addresses;
  - a home relay that always works;
  - data flows at once and moves to a direct path in the background, inside the *same* QUIC
    connection (between Nu nodes);
  - it keeps trying to go direct, and survives network changes.
- **Names are atproto records.** Your handle is your name. `nu publish` writes a small signed
  record into your atproto repo. This replaces IPNS: it resolves in a few HTTPS round trips,
  never expires, survives key rotation, keeps history, and pushes updates.
- **Files are hashed whole** (BLAKE3). The same bytes always get the same CID. Downloads are
  checked in 16 KiB pieces and can come from several peers at once. One DHT record per
  *file*, not per block.

## 2. Background

### 2.1 What go-ipfs got right (keep)

- One binary: `init`, `daemon`, `add`, `cat`, `get`, `pin`. A localhost API and a local
  gateway.
- Content addressing: any byte can be checked, whoever served it.
- Real p2p on a shared, open network (libp2p), not a walled garden.
- Embeddable libraries.

### 2.2 What went wrong in IPFS (fix)

| # | Problem | Root cause | Nu fix |
|---|---------|------------|--------|
| P1 | Finding content is slow | Global DHT walk on the critical path | Names point to publisher nodes directly (§7, D13); DHT as fallback (D15) |
| P2 | Providing doesn't scale | One provider record per *block* (256 KiB), each stored on 20 DHT peers and re-sent every 22h. Provide Sweep cut lookups ~97 % but not the record count | Whole-file hashing → one record per *file or tree* (D2, D15) |
| P3 | Deep DAGs need many round trips | Bitswap asks block by block | Ask for a path/tree in one request, get an ordered CAR (D14) |
| P4 | Same file, different CIDs | UnixFS chunker/layout settings aren't part of the CID | Whole-file BLAKE3 (D2) |
| P5 | Too many formats | CIDv0/v1, many bases, codecs, hashes | DASL/BDASL CIDs only (D1) |
| P6 | IPNS is bad | Slow, expiring, no key rotation, no history, no push | atproto records as names (D4–D7) |
| P7 | No identity | PeerIDs are node keys | DIDs + handles; node keys bound to a DID (D5) |
| P8 | NAT'd peers often unreachable | ~91 % of Amino peers are behind NAT. DCUtR succeeds ~70 % of the time *when it runs*, but ~36 % of DCUtR-capable NAT'd peers never get a relay reservation, and default relays cap circuits at 2 min / 128 KiB (coordination only). There is no fallback that always works, so people use trusted HTTP gateways | Guaranteed home relay with full service for our peers, relay-first dialing, continuous re-punching, port mapping (D9–D12) |
| P9 | Pinning is manual, per CID | No "keep what Alice publishes" | `nu follow`, my-devices replication (D17) |
| P10 | Huge, crufty stack | go-libp2p/Kubo: many transports, muxers, security protocols, options, a resource manager, etc. | A small, fixed libp2p subset, implemented and tested thoroughly (D8) |

### 2.3 What we take from atproto

- **Identity separate from keys and hosting:** DIDs (`did:plc`, `did:web`), with a signing key
  and PDS in the DID document; rotation keys; DNS handles checked both ways.
- **Signed repos:** a Merkle Search Tree (MST) of records with a signed commit at the root.
  `com.atproto.sync.getRecord` returns a small proof CAR. That's what IPNS wanted to be.
- **Infrastructure that already runs:** PDSes (including free Bluesky accounts), PLC (now run
  by the independent PLC Association), relays, Jetstream.
- **OAuth with narrow scopes** (`repo:<collection>`, permission sets).

### 2.4 What we take from iroh (as a model, not a dependency)

iroh (Rust, 1.0 in June 2026) is the best current example of "p2p connections that just work".
We copy its *behaviour* and express it in libp2p-standard protocols:

| iroh idea | Nu equivalent on the libp2p wire |
|-----------|---------------------------------|
| Dial keys, not IPs | `connect(PeerId)` → a peer session that owns every connection to that peer (D9) |
| Every endpoint has a home relay; the relay path always works | Always keep a circuit relay v2 reservation on a Nu relay; Nu relays give full-service circuits to Nu peers (D10) |
| Relay first, direct in the background | Race relayed and direct dials; first success carries data (D9) |
| One connection that migrates between relay and direct paths (magicsock ≤0.35; multipath since 0.96) | Nu↔Nu: negotiate `/nu/qpath/1` on the circuit, run a normal libp2p QUIC connection *over* the relay stream, then move its packets to a direct UDP path found by disco probing, the way magicsock does (D11). Standard libp2p peers: separate relayed and direct connections, bridged by resumable requests (D9) |
| NAT traversal inside QUIC (QNT) | Disco-style continuous probing between Nu nodes (D11); standard DCUtR with iroh-style behaviour for other libp2p peers (later, D11b) |
| QUIC Address Discovery from relays | identify `ObservedAddr` from our relays + AutoNAT v2 confirmation (D10) |
| Portmapper, netwatch | Same functions: UPnP-IGD / PCP / NAT-PMP port mapping and OS network-change monitoring (D12) |
| pkarr records served over DNS | Signed libp2p peer records served as standard **`/dnsaddr`** TXT records and via the **`/routing/v1/peers`** HTTP API (D13) |
| mDNS-style local discovery | libp2p mDNS (on by default) |
| Verified streaming blobs (iroh-blobs, bao-tree) | Same hash (BLAKE3) and verified streaming at 16 KiB chunk groups, over a libp2p stream protocol (D2, D14; exact wire format Q21) |

### 2.5 Other prior art and evidence

- **libp2p DCUtR measurements:**
  - *IMC '26 (Trautwein et al.):* 70 % ± 7.1 % conditional success.
  - *ProbeLab DCUtR report (May 2026):*
    - by NAT type: port-restricted cone 82.9 %, symmetric ~40 %;
    - go-libp2p ↔ rust-libp2p interop is 100 % across every NAT that can be punched;
    - ~36 % of NAT'd peers that advertise DCUtR never get a relay reservation.
- **Ethereum consensus networking profile:** QUIC and TCP both MUST be supported; QUIC is
  primary. QUIC uses libp2p TLS; TCP uses Noise XX with **secp256k1** identities.
  multistream-select 1.0. mplex is MUST and yamux MAY, with yamux preferred when both exist.
  Listening endpoints must be public (no relay/AutoNAT).
- **DASL** (CID, DRISL, BDASL with BLAKE3 verified streaming, RASL); **IPFS trustless gateway**
  (ordered CAR); **BitTorrent** (parallel multi-peer download).
- **Tailscale** (Go, BSD-3): `magicsock`, the `disco` protocol, DERP relays, `portmapper`,
  `netmon`, `netcheck`. It's the production Go reference for relay first, direct later, and
  the design iroh's pre-0.96 magicsock descended from.
- **quic-go** (the QUIC stack go-libp2p uses): lets non-QUIC packets share the QUIC UDP socket
  (`Transport.WriteTo`, `Transport.ReadNonQUICPacket`); GSO and batched receive on Linux.
  Multipath isn't upstream yet (issue #3343).

## 3. Goals and non-goals

**Goals (in priority order)**
1. **It just works.** Given a PeerId or a DID, a connection succeeds on any network where
   HTTPS works, with no configuration. Transfers survive network changes.
2. **libp2p wire compatible:** interoperates with go-, rust- and js-libp2p (Kubo, Helia,
   Ethereum clients) for connections, the Amino DHT, relays and hole punching.
3. **As easy as go-ipfs:** `nu init && nu daemon`, then `add` / `get` / `cat` / `pin`. Works on
   a LAN with no internet.
4. **Names that work:** publish/resolve via an existing atproto account; cold ≤ ~1 s, warm ~0,
   updates pushed.
5. **Everything checked; big files work well** (resumable, multi-peer, verified ranges).
6. **Small and embeddable:** a fixed protocol set, thoroughly tested; the daemon is a thin shell
   over reusable packages.

**Non-goals (v1)**
- Running atproto infrastructure.
- Every libp2p feature (see D8 for what's excluded and why).
- Byte-level compatibility with IPFS *data* (UnixFS/Bitswap): optional bridge only.
- Incentives, anonymity, private sharing (designed in §12, shipped later).

## 4. User experience

```
nu init                              # node key (libp2p PeerId, ed25519), ~/.nu store, config
nu daemon                            # libp2p node + API on :5171 + gateway on :8171
nu doctor                            # UDP reachability, NAT type, relays, port mapping, IPv6

# content (no identity needed)
nu add photo.jpg                     # → bafyr…  (file node: name, mime, size → raw blob bafkr4i…)
nu add --raw photo.jpg               # → bafkr4i…  (just the BLAKE3 hash of the bytes)
nu add -r ./dir                      # → bafyrei…  (tree, DRISL)
nu cat <cid> | nu get <cid> [-o path]
nu ls <tree-cid>[/sub/path]
nu pin add|rm|ls <cid>
nu share <cid>                       # link with PeerId + relay multiaddr hints (§7.3)

# identity (atproto)
nu login alice.example.com           # atproto OAuth; scope = nu.* records only
nu node register [--label laptop] [--seed]

# names
nu publish <name> <cid>
nu resolve alice.example.com/<name>
nu get alice.example.com/<name>[/path]
nu names | history | unpublish

# replication
nu follow alice.example.com[/name]
nu devices

# infrastructure (optional)
nu relay                             # circuit relay v2 server on 443 (+ dnsaddr/peer-routing server)
```

Gateway: `http://localhost:8171/nu/alice.example.com/site/`, `/blob/<cid>`, subdomain mode per
name.

## 5. Architecture

```
            ┌──────────────────────── nu daemon (one binary) ──────────────────────────────┐
  CLI ────► │ API (:5171)                               Gateway (:8171)                    │
            │  ┌────────────┐ ┌─────────────┐ ┌──────────────────┐ ┌────────────────────┐  │
            │  │ nu-core    │ │ nu-names    │ │ nu-fetch         │ │ replicator         │  │
            │  │ CID/DRISL/ │ │ atproto     │ │ multi-peer,      │ │ pins, follow,      │  │
            │  │ MST/CAR/   │ │ resolve,    │ │ resumable ranges,│ │ my-devices, GC,    │  │
            │  │ Bao        │ │ publish     │ │ tree fetch       │ │ providing          │  │
            │  └────────────┘ └──────┬──────┘ └────────┬─────────┘ └────────────────────┘  │
            │  ┌──────────────┐      │       ┌─────────▼────────────────────────────────┐   │
            │  │ store        │      │       │ nu-net (our libp2p subset)               │   │
            │  │ blobs +      │      │       │  peer sessions · dialer race · relays    │   │
            │  │ outboards,   │      │       │  QUIC (libp2p TLS) · magicsock + disco   │   │
            │  │ db meta      │      │       │  identify · ping · AutoNAT v2 · qpath    │   │
            │  └──────────────┘      │       │  circuit v2 · kad · mDNS · portmap ·     │   │
            │                        │       │  netmon · address lookup                 │   │
            │                        │       └─────────┬────────────────────────────────┘   │
            └────────────────────────┼─────────────────┼────────────────────────────────────┘
                                     │ HTTPS           │ libp2p
              ┌──────────────────────▼─────┐   ┌───────▼────────────────────────────────┐
              │ atproto (PDS, PLC, DNS,    │   │ Nu peers · Kubo/Helia · Ethereum and   │
              │ Jetstream)                 │   │ other libp2p nodes · Amino DHT ·       │
              └────────────────────────────┘   │ Nu relays + dnsaddr/peer-routing srv   │
                                               └────────────────────────────────────────┘
```

Two planes: the **name plane** (atproto, small signed records) and the **data plane** (libp2p,
content-addressed bytes from anyone).

## 6. Data layer

### D1. CIDs = DASL/BDASL CIDs
CIDv1, base32 lowercase strings only. Codecs `raw` (0x55) and DRISL (0x71). **BLAKE3 (0x1e)
everywhere**: blobs and DRISL tree nodes. Digest exactly 32 bytes. DRISL blocks ≤ 1 MiB.

**Compatibility (Q1, resolved):**
- *IPLD:* fully valid. These are ordinary CIDv1s with codec `raw` or `dag-cbor` and multihash
  `blake3` (multicodec table: 0x1e, status "draft", default 32-byte output). DRISL is a strict
  subset of DAG-CBOR, so any DAG-CBOR decoder reads our blocks. go-multihash ships a BLAKE3
  hasher, and Kubo/boxo can verify these blocks.
- *DASL:* conformant with **BDASL**, which extends DASL CIDs with hash type 0x1e for either
  codec. Not *core* DASL (SHA-256 only), so strict DASL-only tools reject our CIDs. atproto
  never sees them as links, because we store targets as strings.
- *What IPLD validity doesn't give us:* IPFS nodes can *verify* our blocks but can't *transfer*
  our big blobs, because Bitswap caps blocks at ~2 MiB. See D14.

### D2. Blobs: one file = one BLAKE3 hash, verified streaming
- `CIDv1(raw, blake3(whole file))`. Same bytes → same CID, always.
- Stored with a **Bao outboard** at 16 KiB chunk groups (Q21, resolved).
  - *Bao* is BLAKE3's own verified-streaming format (by BLAKE3 co-author Jack O'Connor).
    BLAKE3 is internally a binary Merkle tree over 1 KiB chunks. Bao serializes that tree's
    interior nodes (64 bytes each: two child hashes), either interleaved with the data
    ("combined") or in a separate file ("outboard").
  - With only the root hash, a client can check any byte range as it arrives. In effect it's
    a balanced UnixFS-style DAG built into the hash function: deterministic, with no chunker
    settings.
  - Grouping 16 chunks (16 KiB) per leaf cuts the outboard from ~6 % to ~0.4 % of the file.
  - We use standard Bao (`lukechampine.com/blake3/bao`, which supports chunk groups), and
    cross-check test vectors with iroh's `bao-tree` so an iroh bridge stays possible.
  - This matches BDASL's streaming-verification rules. Downloads are checked as they stream, byte ranges
  can be checked on their own, several providers can serve one download, and bad peers are
  dropped.
- Blob hashes are identical to iroh-blobs hashes (useful for bridging later), but the transfer
  protocol is our own libp2p stream protocol (D14).

### D3. Trees: one DRISL node per directory, MST, no sharding threshold

*Decided (Q9, Q29). v0.6 proposed one MST keyed by full path; that was reversed for the reasons below.*

**Layout: per directory (like UnixFS), not one map keyed by full path.**
- **Shared subtrees dedupe.** An identical subdirectory (a vendored library, an asset folder)
  has the same CID wherever it appears, across trees and across versions. Full-path keys
  destroy this, because the same files under a different prefix are different keys.
- **Renames and moves are cheap.** Moving a directory with 100k files re-links one CID and
  rewrites only the ancestors. With full-path keys, all 100k keys change.
- **Diffs still skip unchanged subtrees**, now at directory granularity as well as within
  large directories.
- Cost: resolving a path takes one step per path segment (a path proof is a chain of
  directory nodes). The tree protocol streams that chain in a single request anyway (D14).

**Each directory is one deterministic map `name → entry`. The same structure is used at every
size, with no sharding threshold.**
- UnixFS switched from a flat directory to a HAMT at a size threshold. Go and JS estimated
  that size differently over the years, so the same directory got different CIDs (IPIP-499,
  2026, now has to pin the threshold in "CID profiles"). We avoid thresholds entirely.
  A structure that is a single node when small and grows levels as needed gives one encoding
  per entry set.
- Entry: `{ kind: "file"|"dir"|"symlink", cid, size, mode?, mime? }`. `size` is the total
  bytes below the entry, like UnixFS `Tsize`, so progress bars and quotas work before
  fetching.
- Root header: `{ $type: "io.ipfs.nu.tree", v: 1, root: <dir cid>, files, bytes }`.

**Map structure: MST vs CHAMP** (Q29). Both are canonical (history-independent) and both
degrade to a single node for small directories:

| | MST (atproto, Inria) | CHAMP / IPLD HashMap (what UnixFS HAMT approximated) |
|---|---|---|
| Order | **sorted by name**: sorted listing, prefix and range queries, "page after name X" | hash order: listing a huge directory in sorted order means fetching all of it |
| Balance / node size | probabilistic; node size varies; publishers can grind names to make lopsided nodes (only hurts their own tree; capped by the 1 MiB block limit) | tight: fixed fanout (e.g. 32), predictable node size and depth |
| Implementation | insert/delete with splits and merges; atproto needed interop test vectors to get it right | simpler; the canonical form comes from CHAMP's compaction-on-delete rule |
| Parallel sync / cursors | split by name range; cursor = last name | split by hash prefix (evenly balanced); cursor = hash position |
| Ecosystem | same algorithm as our name plane (atproto repos) | IPLD HashMap spec; UnixFS lineage |

**Decided: MST**, with a wider fanout than atproto's (16 or 32; Q27), because sorted order is
what users see (`ls`, gateway listings, paging, datasets used as sorted indexes). CHAMP would
have won on predictability and simplicity; we accept MST's variable node sizes (capped by the
1 MiB block limit) and invest in cross-implementation test vectors. Prolly trees were
considered and not chosen (less standardized).

**Large directories never have to be fetched whole.** A directory *is* its MST. Looking up
one name fetches only the nodes from the MST root down to the leaf holding that name:
O(log_f n), each node a few KB. Rough numbers at fanout 16 and ~100 bytes per entry:

| Entries | Whole directory | Nodes to look up one name | Bytes for one lookup (with proof) |
|---|---|---|---|
| 1 k | ~100 KB | ~3 | ~5 KB |
| 1 M | ~100 MB | ~5–6 | ~10 KB |
| 100 M | ~10 GB | ~7 | ~15 KB |

- `/nu/tree/1` with a `path` streams exactly this chain, for every directory along the path,
  in **one request**. A layer-1-only client needs one round trip per MST level, so fanout
  trades node size against round trips (Q27).
- Listing is paged by name range (`name_range` + `limit`): "the first 100 entries after
  `foo`" costs one MST path plus the covered nodes. Prefix queries cost O(log n + matches).
- Updates, renames and diffs inside a huge directory touch O(log n) nodes.
- The only operations that are O(n) are the inherent ones: a full listing, or copying the whole
  directory.
- Contrast with UnixFS: a directory below the sharding threshold was a single node that had to
  be fetched whole (up to hundreds of KB), and the HAMT above it was hash-ordered, so it
  couldn't be paged in name order.

### D3a. Single files are wrapped (Q30)

`nu add photo.jpg` returns the CID of a tiny DRISL **file node**:

```
{ $type: "io.ipfs.nu.file", blob: <raw BLAKE3 cid>, size, name?, mime?, mode? }
```

- **Why:** a link someone is handed should open properly (filename and content type in
  gateways and on download, and size known before fetching). A bare hash can't carry that.
- **"Same bytes, same hash" is kept one level down:** the inner `blob` CID is always the plain
  BLAKE3 hash of the bytes. Storage, transfer, dedup and the DHT all work on blob CIDs.
  Adding the same bytes under two names gives two small file nodes and **one** stored blob.
  (UnixFS lost this because its wrapping *was* the chunked DAG.)
- `nu add --raw` returns the bare blob CID. Files *inside* directories are not wrapped:
  directory entries already carry name, size and mime and point straight at the raw blob.
- Fetching a file node costs no extra round trip: `/nu/tree/1` with `scope: entry` returns the
  file node and streams the blob (or inlines it if small).
- Providing: announce both the file-node CID and the blob CID (still two records per file,
  not one per block).

### How you know what a CID is

- **The codec in the CID** says bytes vs structure *before* fetching: `raw` (0x55) = opaque
  bytes (a file), DRISL (0x71) = a structured node.
- **Inside a DRISL node, `$type`** says which structure it is (file node, tree header, directory shard,
  and later other types), like atproto's `$type`.
- **Links carry hints** so fetchers can plan without fetching: directory entries carry
  `kind` + `size`, and `nu.name` records carry `kind` + `size`. `/nu/have/1` also answers
  `{codec, size}` for CIDs a peer has.
- **Every CID, raw or DRISL, can be fetched as bytes by hash** (D14 layer 1). A client that
  knows nothing about trees can still fetch and verify anything, then walk the links itself.

## 7. Names: atproto as a better IPNS

### D4. Identity
- **Mode A (default):** existing atproto account via OAuth (DPoP, PKCE, PAR). The scope is a
  Nu permission set `io.ipfs.nu.authPublish` (`repo:io.ipfs.nu.name`, `repo:io.ipfs.nu.node`).
  The PDS signs commits. Namespace `io.ipfs.nu` comes from the domain **nu.ipfs.io** (Q3; it can
  change later). The text writes `nu.name` / `nu.node` for short.
- **Mode B (later):** the daemon holds the signing key and serves its own single-account repo
  (Q4).
- No identity needed for `add`/`get`/`cat`/`pin`/`share`.

### D5. Records
```
nu.name / <name>
{ target: "<cid>", kind: "tree"|"blob", size?, note?, updatedAt }

nu.node / <peerId>                 # rkey = base32 CIDv1 (libp2p-key) form of the PeerId
{ label?, roles: ["device"|"seed"],
  relays?: ["/dns4/relay1…/udp/443/quic-v1/p2p/<relay>/p2p-circuit", …],   # never raw IPs
  dnsaddr?: "/dnsaddr/<peerid>.peers.nu.ipfs.io",
  createdAt }
```
Targets are strings (a PDS doesn't need to understand BLAKE3). A name is
`at://did:…/io.ipfs.nu.name/<name>`. Its history is the signed commits.

### D6. Resolution
Handle → DID (checked both ways) → DID doc (PDS + key) → `getRecord` proof CAR (signature and
`rev` checked; no rollback) → target CID → data plane. Cold: 3–4 HTTPS round trips; warm: 0.

| | IPNS | Nu names |
|---|------|----------|
| Name | hash of a public key | handle → DID |
| Lookup | DHT walk, seconds to minutes | 1–4 HTTPS RTT, usually cached |
| Expiry | 48h | none |
| Key loss | name lost | DID rotation keys |
| Updates | poll | push |
| History | none | signed commits |

### D7. Push updates and following
Jetstream (`wantedDids`, `wantedCollections`) triggers a re-fetch *with proof*. The full signed
firehose is an option. Followers pin new targets per policy and track `rev`.

### 7.3 Links
```
nu://alice.example.com/site/docs/intro.html
nu://did:plc:abc…/site@<rev>
nu://bafyrei…/docs/intro.html
nu://bafyrei…?via=did:plc:abc…&peer=/p2p/<peerid>&relay=<circuit multiaddr>
rasl://bafkr4i…/?hint=host.example
```

## 8. Networking: a libp2p subset that behaves like iroh

The approach: **with any libp2p peer, the wire is libp2p, strictly to spec. Between two Nu nodes,
extensions are negotiated through multistream-select, so libp2p peers never see them, and they
give iroh-grade behaviour.**

### D8. The libp2p subset (v1: QUIC only)

| Area | v1 | Later / never |
|------|----|---------------|
| Transports | **QUIC-v1 only** | TCP, WS/WSS (Q19); WebTransport, WebRTC-direct (browsers, Phase 3). mplex doesn't apply without TCP |
| Security | libp2p **TLS 1.3** (inside QUIC) | Noise: only needed later, to secure relayed connections with non-Nu peers (D11b) |
| Muxing | QUIC's own streams | yamux: later, same reason as Noise |
| Negotiation | multistream-select 1.0 with optimistic (lazy) select | multiselect 2 (unfinished) |
| Keys | own key ed25519; *verify* ed25519, secp256k1 (Ethereum), ECDSA, RSA (older IPFS nodes) | — |
| Core protocols | identify (+push), ping, **AutoNAT v2** (client + server), **circuit relay v2** (reservation client, stop, hop server), **kad-dht** (Amino; client, and server when public), **mDNS**, signed peer records | AutoNAT v1, circuit v1, rendezvous, floodsub, peer exchange |
| Later / optional | DCUtR with non-Nu peers (D11b), gossipsub, bitswap (IPFS bridge), HTTP over libp2p | — |
| Resource control | simple per-peer / per-protocol / per-connection limits | go-libp2p's resource-manager model |

**What QUIC-only gets us:** QUIC-v1 is the default listener on Kubo and a MUST (and the
primary transport) for Ethereum consensus clients. So v1 can dial and accept with the public
libp2p world, join the Amino DHT, and reach relays.

**What it costs**, and when it gets fixed:
1. Networks that block UDP entirely (some corporate and guest Wi-Fi) can't connect at all
   until relays also accept WSS/TCP on 443 (Q19). This is the biggest "just works" gap in v1,
   and `nu doctor` must say so plainly.
2. libp2p nodes that only listen on TCP are unreachable (rare among public nodes).
3. Standard libp2p peers behind NAT are reachable only after D11b (Noise + yamux over
   circuits).

### D9. Peer sessions: dial peers, not addresses

- `connect(PeerId)` returns a **session** that owns every connection to that peer and puts new
  streams on the best path.
- **Between Nu nodes, a session is normally one QUIC connection** whose packets move between
  relay and direct paths underneath (D11). The connection survives path changes and network
  changes.
- **With other libp2p peers**, a session may hold a relayed and a direct connection side by
  side. Data-plane requests are small and resumable (D14), so moving to the better connection
  never restarts a transfer.
- **Dial race:** all known ways to reach the peer start concurrently, with short staggers (LAN
  → IPv6 → public IPv4 → relay). The first success carries data.

### D10. Home relays that always work

- Every Nu node that isn't publicly reachable **always holds a circuit relay v2 reservation** on
  a home relay, plus a backup. They are picked by latency, renewed before the TTL, and replaced
  on failure; changes go out through identify push and the node's peer record.
- **Nu relays are standard circuit relay v2 hop servers** (`nu relay`). v1 listens on
  **QUIC/UDP 443**; WSS/TCP 443 comes later (Q19).
  - Admitted Nu peers get **full-service circuits**: no 2 min / 128 KiB cap (the spec's limit
    is optional).
  - Everyone else gets standard limited circuits, which is all the libp2p network expects.
  - Relays also run identify and an AutoNAT v2 server, so a node learns and confirms its public
    UDP address within one round trip of starting.
- Who runs the default relays, and admission policy: **deferred** (Q10). During development we
  run our own.

### D11. Nu ↔ Nu: one QUIC connection over relay and direct paths ("nu-path")

This is the magicsock + disco design that iroh used up to 0.35 and that Tailscale runs in Go,
placed underneath an ordinary libp2p QUIC connection.

1. **Reach B via its relay (standard).** A opens a circuit to B through B's home relay
   (circuit v2 CONNECT → STOP). On the relayed stream, A proposes the security protocol
   `/nu/qpath/1` via multistream-select. A non-Nu peer answers `na` and A falls back to the
   standard path (D11b). **The libp2p wire stays standard for everyone else.**
2. **QUIC over the relay.** Once `/nu/qpath/1` is accepted, the relayed stream carries framed
   QUIC packets.
   - A's *magicsock* (a `net.PacketConn` wrapper under the QUIC stack) gives B a stable
     virtual address.
   - A dials an ordinary **libp2p QUIC connection** (libp2p TLS, ALPN `libp2p`) to that
     address. Its packets travel over the relay stream.
   - Peer authentication and encryption are QUIC's own, end to end; the relay sees only
     ciphertext.
3. **Path discovery (disco):**
   - The peers exchange candidate addresses (LAN, observed, port-mapped, global IPv6) on a
     control stream `/nu/path/1`.
   - Both then keep sending small probe packets to each other's candidates. Probes are
     non-QUIC packets on the same UDP socket (`Transport.WriteTo` / `ReadNonQUICPacket` in
     quic-go).
   - Probes are MAC'd with a key derived from the QUIC TLS session (TLS exporter), so nobody
     else can forge them.
4. **Switch paths underneath QUIC.** The first candidate pair that completes a probe round trip
   becomes the direct path, and magicsock starts sending the connection's packets there.
   QUIC sees the same virtual address and routes by connection ID, so nothing reconnects. The
   relay path stays warm.
5. **Heartbeats and fallback.** Direct paths are probed on a heartbeat. On loss, or on a network
   change, packets go back over the relay immediately and probing starts again. A transfer
   carries on through a Wi-Fi → cellular switch.
6. **Keep upgrading.** Probing starts again when addresses change, a new observed address or a
   port mapping appears, or a backoff timer fires.

**Why this rather than DCUtR between Nu nodes:**
- DCUtR produces a *second* connection, depends on RTT/2 timing, and runs once.
- Disco probing is continuous, needs no timing, and keeps one connection.
- It's the approach behind iroh's and Tailscale's reliability.

**What we give up compared with iroh 1.0:**
- Per-path congestion control, and hole-punch packets carried inside QUIC (QUIC multipath +
  QNT). Both need QUIC extensions, i.e. a forked QUIC stack in Go *or* Rust: iroh forked quinn
  into noq to get them.
- We revisit if quic-go gets multipath upstream (Q20).

**Performance note:** magicsock wraps the UDP socket. It must implement quic-go's OOB-capable
packet-conn interface so GSO, ECN and batched receive keep working.

### D11c. Hole punching: maximum effort

Goal: **direct wherever it is physically possible**, and the best possible relay where it
isn't. Tailscale reports >90 % direct with an approach close to D11. The cases it loses are the
"hard" ones: symmetric (endpoint-dependent mapping, EDM) NATs, CGNAT, double NAT, and strict
firewalls. That's where the extra effort goes. The escalation ladder below runs automatically
and in the background. Data always flows over the relay in the meantime, so extra effort
costs latency to *direct*, never time to first byte.

**1. Know your own network (continuous "netcheck").** Measured at startup, on every network
change, and periodically:
- **Mapping behaviour:** compare the observed address reported by ≥ 3 observers on different
  IPs *and* ports (our relays, plus any public peers) → endpoint-independent (EIM),
  address-dependent, or address+port-dependent (EDM).
- **Port allocation pattern** for EDM NATs:
  - *port-preserving* (external port = local port);
  - *sequential* (next port = last + δ, with δ and jitter measured by rapid back-to-back
    probes);
  - *random*.
- **Filtering behaviour:** relays can answer from an alternate IP/port on request (as in STUN
  RFC 5780 CHANGE-REQUEST), which tells us whether unsolicited replies get through.
- **Mapping lifetime:** idle-then-probe measurements; sets keepalive intervals per network
  (default 25 s, shorter on CGNATs with short timeouts).
- Also: hairpinning, IPv6 availability and firewall behaviour, UDP blocked or not, and a port
  mapping protocol present or not.
- The result is a compact **NAT profile**, exchanged with peers during candidate exchange
  and shown in `nu doctor`.

**2. Gather every candidate.**
- LAN addresses on all interfaces, including secondary ones (Wi-Fi + Ethernet + cellular).
- **Global IPv6**, preferred: often no NAT at all, just a stateful firewall that punches
  trivially.
- Observed (reflexive) addresses.
- Router-mapped addresses via **PCP / NAT-PMP / UPnP-IGD**, including PCP firewall pinholes
  for IPv6 and PCP `PEER` mappings where a CGN supports PCP. Watch for router reboots (mapping
  epoch) and re-map.
- **Predicted** addresses for sequential/port-preserving EDM NATs.
- NAT64-synthesised addresses on IPv6-only mobile networks.

**3. Probe smartly (disco, ICE-style pair ordering).**
- Both sides probe all candidate pairs at once, best pairs first: LAN → IPv6 → mapped → EIM
  reflexive → predicted.
- Aggressive schedule for the first few seconds (about 0, 20, 50, 100, 200, 500 ms, 1 s, 2 s),
  then back off.
- **Never give up while relayed.** Re-attempt on every address, profile or network change,
  and on a timer (30 s → 2 min → 10 min).
- Optional **low-TTL priming**: the first probes use a TTL just large enough to open our own
  NAT's mapping but not reach the far NAT. Some NATs penalise unsolicited arrivals (ICMP,
  blacklisting). Kept or dropped based on NAT lab results.

**4. Escalate by NAT pair.** The two sides' NAT profiles choose the strategy:

| Pair | Strategy | Expected |
|------|----------|----------|
| Either side public, IPv6 without NAT, or router-mapped | direct dial to that side | ~always |
| Same NAT / LAN | LAN candidates, mDNS, hairpin | ~always |
| EIM ↔ EIM (cone ↔ cone) | simultaneous probing | ~always (DCUtR measured 82.9 % for port-restricted cone *with one attempt*; we retry indefinitely) |
| EIM ↔ **predictable** EDM (port-preserving / sequential) | **port prediction**: the EDM side allocates toward the peer and reports its predicted port; the EIM side probes a window around it (±64…256) | high; the literature reports ~76–78 % for "progressing" symmetric NATs with one method, more with windows + retries |
| EIM ↔ **random** EDM | **birthday spraying**: the EDM side opens 256 sockets, each sending to the EIM side's address (256 mappings). The EIM side probes random ports on the EDM IP | 256 probes → 64 %; 1024 → 98 %; 2048 → 99.97 % (about 200 KB of probes over a few seconds) |
| EDM ↔ EDM, at least one predictable | prediction on the predictable side, spraying on the other | medium |
| EDM ↔ EDM, both random | same search, but the space is 2³² pairs: ~65k probes per side gives only 64 %. **Not attempted by default**; best relay (step 5) | relay |
| UDP blocked on either side | relay over WSS/443 once Q19 lands | relay |

Guardrails for prediction and spraying: they look like port scans to intrusion-prevention
systems and use NAT table space (CGNATs often cap ports per subscriber). So they:
- run only between two Nu nodes that both agree to it (negotiated in `/nu/path/1`), never
  toward other peers;
- run only after cheaper methods fail;
- are rate-limited per peer and globally, with the spray socket count capped (default 256)
  and sockets released immediately afterwards;
- back off on ICMP errors or IPS-like drops;
- can be disabled in config (`holepunch.aggressive = false`).

**5. When direct is impossible, use the best relay, not just any relay.**
- **Datagram relaying:** Nu relays offer a Nu-only extension that forwards inner QUIC packets
  as **QUIC datagrams** (TURN-like) instead of bytes on a reliable stream. This avoids
  QUIC-in-a-reliable-stream head-of-line blocking and "TCP meltdown". The standard circuit v2
  stream is the fallback, and remains all other libp2p peers ever see.
- **Relay choice by pair:** pick the relay minimising RTT(A→relay) + RTT(relay→B), not just
  A's home relay.
- **Peer relays:** any publicly reachable Nu node (a seed, a friend's server) can offer
  relaying for its owner's devices or an allow-list (like Tailscale Peer Relays). Often far
  closer than project relays.
- Keep trying to go direct (step 3), and move as soon as it works.

**6. Measure everything.**
- **NAT lab** (D16) extended with *port allocation behaviours* (preserving, sequential with
  jitter, random), mapping timeouts, CGNAT port quotas and IPS-like drop rules.
- A **real-hardware lab:** a handful of consumer routers, OpenWrt, pfSense/OPNsense with and
  without endpoint-independent NAT, and a phone hotspot on two carriers.
- **Opt-in field telemetry:** NAT profile pair → strategy → outcome and time to direct.
- Targets:
  - direct for every pair where at least one side is EIM, public, mapped or on IPv6;
  - ≥ 98 % for EIM ↔ random-EDM;
  - a published, per-pair success table, re-measured every release.

### D11b. Nu ↔ standard libp2p peers behind NAT (later): DCUtR

The relayed connection is upgraded the standard way (Noise + yamux over the circuit), DCUtR
runs on it, and a direct QUIC connection is dialed. We wrap it in iroh-style behaviour:
- gather candidates early (observed addresses confirmed by AutoNAT v2, port mapping, IPv6);
- one UDP socket for everything;
- swap roles and retry with jitter;
- **re-run DCUtR while still relayed** (the libp2p stacks try once);
- classify our own NAT type, then use it to choose roles and show it in `nu doctor`;
- port prediction from D11c where our side is predictable. Spraying isn't possible here:
  DCUtR has no way to negotiate it.

Target: at least the measured baseline (82.9 % for port-restricted cone, 70 % overall).
Phase 3.

### D12. Network changes

An OS network monitor detects interface or route changes, sleep/wake and VPN toggles. The node
then:
1. rebinds its sockets;
2. confirms its addresses again (AutoNAT v2) and refreshes port mappings;
3. renews its relay reservation and sends identify push to connected peers;
4. restarts probing.

nu-path connections survive a change by falling back to the relay path. Other sessions
resume at the request level (D9). Port mapping supports UPnP-IGD, PCP and NAT-PMP. Reference
code: Tailscale's `portmapper` / `netmon` in Go (BSD-3), n0's `portmapper` / `netwatch` in Rust.

### D13. Finding a peer's addresses (iroh's address lookup, in libp2p formats)

All sources run in parallel; dialing starts as soon as any one answers:

1. **Cache** of recent addresses.
2. **Hints** in links (`peer`, `relay` circuit multiaddrs).
3. **`nu.node` records** (relay circuit addresses + dnsaddr name) for "Alice's nodes".
4. **dnsaddr** (the equivalent of iroh's pkarr-over-DNS): each node uploads a *signed libp2p peer
   record* to a lookup server. The server checks it and publishes TXT records at
   `_dnsaddr.<peerid-base36>.peers.nu.ipfs.io`. So
   `/dnsaddr/<peerid>.peers.nu.ipfs.io/p2p/<peerid>` resolves **in any libp2p implementation that
   supports dnsaddr**. (base36 CIDv1 PeerIds are lowercase and fit in a DNS label.)
5. **Delegated peer routing:** the same server answers `GET /routing/v1/peers/{peerid}`
   (IPIP-417).
6. **mDNS** (on by default): a LAN with no internet still works.
7. **Amino DHT** `FIND_PEER`.

### D14. Sync protocols (replacing Bitswap / Graphsync for Nu data)

*Decided: two layers; binary framing on libp2p streams (Q13).*

**What has to move:**
1. **Blobs:** one BLAKE3 hash each, KB to TB. Need verified streaming, ranges, several sources
   at once, resume.
2. **Trees:** DRISL MST nodes (small). Need: whole tree, one path, or **the difference from a
   version I already have** (followers updating a site).
3. **Presence:** which connected peers have which CIDs, or which ranges of them.

Names and their updates travel over atproto, not the data plane.

**Lessons from what exists:**

| Protocol | Got right | Got wrong (for us) |
|----------|-----------|--------------------|
| **Bitswap** | Simple; works for any DAG; many sources at once (different blocks from different peers); resume = ask for the missing blocks again | Learns the DAG one level per round trip; broadcasts WANT_HAVE to every connected peer (bandwidth, privacy); ~2 MiB block cap, which forced chunking (and so P4); no ranges; per-block overhead. **It can't carry a whole-file BLAKE3 blob at all.** |
| **Graphsync** | One request walks the whole DAG (1 RTT) | IPLD selectors are general, hard to implement, and hard to bound in cost (DoS); one source per request; re-sends blocks you already have (do-not-send was bolted on); hard to resume; little adoption outside Filecoin, which itself moved to HTTP retrieval |
| **Trustless gateway CAR** (IPIP-402) | Small parameter set (path, `dag-scope`, `entity-bytes`); ordered CAR; HTTP/CDN friendly | Single source; re-sends what you have; UnixFS-centric |
| **iroh-blobs** | Bao-verified ranges; several providers; very efficient for big data | Only flat collections (hash sequences); no structured diffs |
| **atproto `getRepo?since=`** | Sends only the diff | Only works if the server knows your version |

**Proposal: two layers, like Bitswap + Graphsync, done differently.** A universal primitive
that is correct on its own, plus an *optional* accelerator. All requests are bounded and
resumable.

*Layer 1: bytes by hash (always enough on its own)*

1. **`/nu/bytes/1`: any CID's bytes, or a range of them.**
   - Keyed by the BLAKE3 digest, so it works the same for raw files and DRISL nodes. A DRISL
     node is just a small byte string with a hash.
   - Request `{hash, ranges}`, where ranges are in 16 KiB chunk groups. Response: a Bao slice
     (data + proof nodes), checked as it streams.
   - Unlike Bitswap there's no block size cap: a 1 KiB directory node and a 100 GB file use the
     same request.
   - A client that implements only this protocol can fetch and verify any Nu data, walking
     DRISL links itself (one round trip per level, like Bitswap).
   - The fetcher splits large blobs across several peers, BitTorrent style (parallel ranges,
     an "endgame" phase that re-requests the last ranges from several peers). Bad data is
     detected within 16 KiB and that peer is dropped.
   - Serving costs a sequential disk read plus an outboard read. It's as cheap as a CDN.

*Layer 2: tree sync (an optional accelerator; removes round trips, never needed for
correctness)*

2. **`/nu/tree/1`: tree sync.**
   - Request `{root, path?, name_range?, limit?, scope: node|entry|subtree, have?: [roots],
     inline_max?: bytes, cursor?}`.
   - The response is a stream of DRISL nodes in fixed pre-order: first the chain of directory
     nodes along `path`, then the subtree. Every node is verified by its hash as it arrives.
   - **`have`:** trees are deterministic, so identical subtrees (whole directories, or shards
     of big directories) have identical CIDs. The server walks the requested tree and the
     `have` tree side by side, skips every shared subtree, and sends only the changed nodes:
     an atproto-style diff without per-client state. If the server doesn't have the `have`
     tree, the client falls back to descending itself, fetching only changed nodes.
   - **`name_range`:** splits a huge directory across several peers (each range carries its
     boundary proofs). Graphsync couldn't do this.
   - **`inline_max`:** small blobs (≤ one 16 KiB chunk group, checked by hashing them whole)
     are sent inline. A 10k-file site of mostly small files then arrives in one request.
   - **Bounded:** each response stops at a node/byte budget and returns a `cursor`. Resuming
     on another connection or peer just resends the cursor (cursor = path + last name). The
     traversal is always a fixed-order walk of a Nu tree, never a general selector, so the
     server's cost is predictable.
3. **`/nu/have/1`: presence (supports both layers).**
   - Request `{cids[], blob range queries}` → bitmap, `{codec, size}` for held CIDs, plus the
     ranges held for partial blobs.
   - Sent only to *candidate* peers (connected peers in the same name context, DHT providers,
     peers from hints), **never broadcast** the way Bitswap's WANT_HAVE is.

**Bitswap stays only in the IPFS bridge** (client, to fetch IPFS content). Nu data is not served
over Bitswap: whole-file blobs are over its block cap, and trees alone are useless without the
blobs. Graphsync: not used.

**Framing (Q13, decided): binary messages on libp2p streams.**

```
→ /multistream/1.0.0\n /nu/bytes/1\n                   (optimistic select: no extra RTT)
→ <uvarint len> DRISL {hash, ranges: [[0,64]]}          ~50 bytes
← <uvarint len> DRISL {status: 0, size: …, meta?}       ~20 bytes
← body: Bao slice | ordered DRISL node stream            then FIN
```

- One QUIC stream per request. Cancel = reset the stream. Backpressure = QUIC per-stream flow
  control.
- Requests and response headers are DRISL maps. Unknown keys are ignored, and breaking changes
  get a new protocol ID (`/nu/bytes/2`).
- Resume needs no server state: tree traversal order is deterministic, so the client computes
  the cursor from the last node it verified.
- Hard limits: request size, response budget per request, and concurrent requests per peer and
  per protocol.
- **Why binary:** minimal per-request overhead, exact semantics, one codec throughout, and the
  same style as the libp2p protocols we already implement. With nu-path and home relays,
  peers are reachable directly or via relay, so we don't need HTTP for connectivity in v1.
- **HTTP is deferred.** A mapping onto HTTP (trustless-gateway style,
  `GET /bytes/<cid>` + `Range` per BDASL, `GET /tree/<cid>/<path>?…`) can be added later for
  public seeds, browsers or UDP-blocked networks, with the same meaning. Options considered
  and not chosen: HTTP/1.1 over libp2p (libp2p HTTP spec), HTTP/3 on a separate QUIC
  connection, and running both encodings from day one.

### D15. Finding peers that have a CID

1. **Hints** (`peer=`, `via=` → `nu.node`).
2. **Name context** (the publisher's nodes, seeds first).
3. **Connected + LAN peers** (`/nu/have/1`).
4. **Amino DHT provider records**, one per blob or tree root: 1 TB / 20k files = 20k records,
   not ~4M. (Check that Amino accepts BLAKE3 multihash keys; Q2.)
5. **Delegated routing** `GET /routing/v1/providers/{cid}`.

**Multi-peer download:** ranges from several peers at once, each checked, bad peers dropped,
completed ranges served onward straight away.

### D16. "Just works" is an engineering requirement

- **Zero config:** relays and lookup servers preconfigured, mDNS and port mapping on, DHT client
  (server when public), plain-English errors.
- **NAT lab in CI:** network namespaces with simulated NATs covering every RFC 4787 mapping ×
  filtering combination, CGNAT, IPv6-only and hairpinning, plus **network changes during
  transfers**.
  - UDP-blocked is in the matrix as an *expected failure* until Q19 lands. It must produce a
    clear error.
  - Pairs tested: Nu↔Nu (nu-path), Nu↔go-libp2p, Nu↔rust-libp2p.
- **Interop CI:** libp2p `unified-testing` transport suite (`quic-v1`); Kubo, Helia and an
  Ethereum consensus client as black-box peers (connect, identify, ping, kad).
- **`nu doctor`:** UDP reachability, NAT class, relay latencies and reservation state,
  port-mapping result, IPv6, and per-session path history ("why is X relayed?").
- **Metrics:** direct vs relayed bytes, time to first byte, time to direct, path switches,
  reservation uptime.
- **Targets:** first byte ≤ relay RTT + handshake; 100 % of connections wherever UDP reaches a
  relay; Nu↔Nu direct rate comparable to iroh/Tailscale on the same NAT matrix.

## 9. Storage, pinning, GC

### D17. Store and replication
- Blob store: files + Bao outboards (small blobs inline); `--nocopy` references files in
  place. Metadata in an **embedded DB** (bbolt or pure-Go SQLite): DRISL blocks, pins, name cache, follow state, peer and address
  cache.
- Pins: recursive/direct, tagged by source (explicit / follow / my-names). Mark-and-sweep GC
  with a size watermark.
- My devices: `seed` nodes follow the owner's DID; publish from the laptop → the NAS pins it →
  the laptop can sleep.
- Providing: pinned blob, file-node and tree-root CIDs → Amino DHT (+ delegated routing if
  configured). Never interior directory shards.

## 10. Local API, gateway, browsers

- API on `127.0.0.1:5171` (JSON over HTTP, versioned, OpenAPI; loopback only unless a token is
  set).
- Gateway on `:8171`: `/nu/<handle>/<name>/<path>`, `/tree/<cid>/…`, `/blob/<cid>`; subdomain
  mode per name. (Public mode for seeds, with HTTPS, RASL and `Range`, comes with the deferred HTTP
  mapping, D14.)
- **Browsers (Phase 3):** public seeds and relays add **WebTransport** and **WebRTC-direct**
  listeners, and WSS with certificates (optionally via libp2p's AutoTLS / `libp2p.direct`, as
  Kubo does). Helia/js-libp2p browsers and a future Nu WASM client can then reach them
  directly.

## 11. Implementation

### D18. Code layout (Go)

```
cmd/nu/                 CLI + daemon entrypoint
core/                   cid, drisl, mst (arbitrary path keys), car (ordered), tree, proofs,
                        blob (BLAKE3 + bao outboards at 16 KiB chunk groups). No I/O.
p2p/wire/               PeerId + key types, multiaddr subset, multistream-select, libp2p TLS
                        certificate extension, protobuf messages
p2p/magicsock/          PacketConn under quic-go: virtual peer addresses, path selection,
                        relay-stream path, disco probes (OOB/GSO-capable)
p2p/host/               quic.Transport, peer sessions, dial race, connection manager, limits
p2p/proto/              identify, ping, autonat v2, circuit v2 (client + hop), mdns,
                        nu/qpath, nu/path
p2p/portmap/ p2p/netmon/  UPnP-IGD/PCP/NAT-PMP and network-change monitoring (adapted from
                        Tailscale, BSD-3)
p2p/lookup/             cache, hints, dnsaddr, routing/v1, DHT
kad/                    Kademlia (Amino-compatible)
proto/                  /nu/bytes/1, /nu/tree/1, /nu/have/1
store/ names/ fetch/ node/ api/ gateway/ client/
relay/                  circuit v2 hop server + dnsaddr/routing server (`nu relay`)
bridge/                 (build tag, later) Noise + yamux over circuits, DCUtR, bitswap, UnixFS
```

**Dependencies:** `quic-go` (stock), `indigo` (atproto identity/crypto/OAuth/repo proofs),
`lukechampine.com/blake3` (Bao with chunk groups), `go-cid`/`go-multiaddr` (small leaf modules;
**never go-libp2p**), a pure-Go embedded DB (bbolt or `modernc.org/sqlite`), `miekg/dns`
(dnsaddr server), Prometheus client. One static binary, no cgo. **Linux (amd64, arm64) only in
v1. MIT licensed.**

Tailscale's `portmapper`/`netmon` code is adapted, not imported: the `tailscale.com` module is
far too large to depend on.

## 12. Privacy and sharing modes

| Mode | How | Who can find it |
|------|-----|-----------------|
| Published | `nu publish` | Everyone (public record, on the atproto firehose) |
| Announced | `nu add` (default) | Anyone with the CID (DHT) |
| Unlisted | `nu add --no-announce` + `nu share` | Link holders |
| Private (later) | encrypt before adding | Key holders |
| Private names (later) | atproto Spaces | Space members |

- `nu.node` records list only relay circuit addresses and a dnsaddr name, never IPs.
- Signed peer records (dnsaddr) can be set to relay-only.
- Relays see which peers talk to which, and how much, but not the content.
- DHT lookups reveal interest in CIDs, as in IPFS.

## 13. Compatibility

- **libp2p:** wire compatible for the D8 subset (QUIC-v1 in v1). Tested against go-, rust- and
  js-libp2p. Nu↔Nu extensions (`/nu/qpath/1`, `/nu/path/1`) are negotiated through
  multistream-select, so other peers simply decline them.
- **IPFS:** joins the Amino DHT, uses circuit relays, and peers with Kubo/Helia over QUIC.
  *Data* compatibility (UnixFS/Bitswap) is available through the optional bridge, later.
- **Ethereum:** connection-level interop over QUIC (libp2p TLS; secp256k1 peer keys verified),
  identify, ping. Consensus clients MUST listen on QUIC, so no TCP is needed. Nu is not an
  Ethereum node. gossipsub is a later optional feature.
- **atproto:** Nu is an ordinary atproto client app.
- **iroh:** not wire compatible (different handshake). Blobs use the same BLAKE3/bao hashes, so
  a bridge is possible.
- **Web / DASL:** public seeds serve HTTPS + RASL.

## 14. Rough numbers (targets to measure)

| Operation | Kubo today | Nu target |
|-----------|-----------|-----------|
| Reach a NAT'd peer | fails if no reservation (~36 % of DCUtR-capable NAT'd peers); relay capped at 2 min / 128 KiB | 100 % via home relay (where UDP works); Nu↔Nu direct rate comparable to iroh/Tailscale |
| Relay → direct transition | new connection via DCUtR, once | same connection, continuous probing (Nu↔Nu) |
| Resolve a mutable name, cold / warm | IPNS: seconds to minutes / TTL cache | ≤ 1 s / ~0 |
| Provider records for 1 TB / 20k files | ~4M | ~20k |
| Fetch a 10k-file site | many Bitswap rounds | one ordered CAR + parallel blob fetches |
| 10 GB from 5 peers | per-block Bitswap | parallel checked ranges |

## 15. Risks

1. **We own a libp2p implementation and a magicsock.** Both are security-critical networking.
   Mitigations:
   - a strict subset;
   - stock quic-go and `crypto/tls` (no forks);
   - Tailscale's production Go code as the reference for magicsock/disco/portmap/netmon;
   - fuzzing of every parser; interop suites and the NAT lab in CI.
2. **UDP-blocked networks don't work in v1** (QUIC-only). This is the largest known gap in
   "just works"; closed by Q19.
3. **QUIC over a relay stream (nu-path).** QUIC-in-a-stream suffers head-of-line blocking and
   a congestion controller that doesn't know the path changed. iroh ≤0.35 and Tailscale lived
   with the same trade-off. The fix is QUIC multipath (Q20).
4. **Relay costs:** full-service relaying carries real traffic until a direct path exists.
   Deferred (Q10).
5. **Amino DHT and BLAKE3 keys** (Q2).
6. **Dependence on atproto** for names (cached names and CID fetch keep working during outages).
7. **Public-by-default names** need clear UX.

## 16. Changes

**v0.6 → v0.7**
- Trees: one MST per directory, no sharding threshold (D3). Large-directory lookups cost
  O(log n) nodes.
- Single files are wrapped in `io.ipfs.nu.file` nodes (D3a).
- Sync: two layers, with binary DRISL framing on libp2p streams; HTTP deferred (D14).
- **D11c, maximum-effort hole punching:** NAT profiling, pair-specific strategies (port
  prediction, guard-railed birthday spraying), datagram relays, pair-aware relay choice, peer
  relays, real-hardware lab and telemetry. New Phase 0b+.
- Accepted leans: Q4, Q6, Q8, Q17 (superseded by D11c), Q23, Q26, Q27, Q28.

**v0.5 → v0.6**
- Decided: domain `nu.ipfs.io` (NSIDs `io.ipfs.nu.*`), MIT license, Linux only for v1,
  BLAKE3 for all CIDs (D1), standard Bao (D2), no Kubo RPC API compatibility.
- D14 rewritten: sync protocol design (blob ranges, tree sync with `have` diffs + key ranges +
  cursors, targeted presence queries). Bitswap stays in the bridge only; Graphsync is not
  used.
- Follow-up discussion: trees become **per-directory** maps with no sharding threshold (MST vs
  CHAMP still open, Q29). Sync is split into **layer 1, bytes by hash (`/nu/bytes/1`, any CID)**
  and **layer 2, tree sync as an optional accelerator**. Added "how you know what a CID is" (D3),
  Q30 (single-file wrapping).

**v0.4 → v0.5**
- **QUIC-only transports in v1.** TCP, WSS and mplex dropped for now; Noise and yamux are only
  needed later, for relayed connections with non-Nu peers (D11b).
- **nu-path (D11):** Nu↔Nu connections run as one libp2p QUIC connection whose packets move
  between the relay stream and direct UDP (magicsock + disco). It's negotiated, so the wire
  with other libp2p peers is unchanged. DCUtR moves to later (D11b), for standard libp2p peers.
- **Language: Go** (Q15, confirmed). Every trick we need works with stock quic-go, and
  Tailscale is the Go reference implementation. Code layout D18 rewritten for Go.
- **Default relays deferred** (Q10).

**v0.3 → v0.4:** libp2p wire compatibility became a hard requirement, and iroh became a
behavioural model only. Peer sessions, guaranteed home relays, dnsaddr/routing lookup, our own
`/nu/blob/1`, and the Amino DHT for content discovery.

**Earlier:** v0.3 moved to Rust + iroh. v0.2 made the daemon + CLI the product and made names
atproto records. v0.1 was a PDS/relay network design.

## 17. Open questions

**Open**
- **Q2 (verification task):** confirm Amino servers accept provider records for BLAKE3
  multihash keys. If not, fall back to delegated routing or a Nu kad protocol ID.
- Everything else is either decided (below) or deferred.

**Later / deferred**
- **Q10.** Default relays and lookup servers: operators, funding, admission. Development uses
  our own.
- **Q19.** TCP-based transports (WSS on 443 first) for UDP-blocked networks. Phase 3.
- **Q18.** Browser listeners. Phase 3.
- **Q20.** QUIC multipath for Nu↔Nu paths if quic-go gains it.
- **Q7.** PDS as fallback seed. Lean: no.

**Resolved**
- **Q1 → BLAKE3 everywhere.** IPLD-valid (CIDv1 + raw/dag-cbor + blake3 multihash) and
  BDASL-conformant, but not core-DASL (D1).
- **Q3 → domain `nu.ipfs.io`:** NSIDs `io.ipfs.nu.*`, peer names `*.peers.nu.ipfs.io`. Can
  change later.
- Q5 → `nu.node` lists relay circuit addresses only, no IPs.
- Q11 → IPFS data bridge is optional.
- Q12 → no iroh-blobs dependency.
- Q14 → indigo.
- Q15 → Go.
- Q16 → no TCP in v1, so no mplex.
- **Q21 → standard Bao, 16 KiB chunk groups** (test vectors cross-checked with iroh's bao-tree).
- **Q22 → no Kubo RPC API compatibility.** Wire compatibility only.
- **Q24 → MIT.**
- **Q25 → Linux only for v1**; macOS, Windows and mobile later.
- **Sync shape → two layers:** `/nu/bytes/1` (any CID by hash) + `/nu/tree/1` (optional
  accelerator) + `/nu/have/1` (D14).
- **Q13 → binary DRISL messages on libp2p streams.** HTTP mapping deferred until needed (D14).
- **Q9 → one node per directory** (D3).
- **Q29 → MST**, fanout still Q27 (D3).
- **Q17 → try as hard as possible:** NAT profiling, port prediction, birthday spraying
  (guard-railed), datagram relays and peer relays (D11c).
- **Accepted leans:**
  - Q27 → fanout 16, confirmed by benchmarks;
  - Q6 → range list (bitmap if fragmented);
  - Q23 → DHT server when publicly reachable, with limits;
  - Q28 → keep the previous version of each followed name;
  - Q8 → flat rkeys, no aliases in v1;
  - Q4 → Mode B in Phase 4;
  - Q26 → 0600 files in `~/.nu`.
- **Q30 → single files wrapped** in an `io.ipfs.nu.file` node by default; `--raw` for the
  bare hash (D3a).

## 18. Phasing

| Phase | Scope | Exit criteria |
|-------|-------|---------------|
| **0a – Wire** | `p2p/wire`, `p2p/host`: QUIC-v1 with libp2p TLS, multistream-select, identify, ping, key types; fuzzing; `unified-testing` quic-v1 interop | Passes transport interop with go-, rust- and js-libp2p; connects to Kubo and an Ethereum consensus client over QUIC |
| **0b – Connectivity** | circuit v2 (client + `nu relay`), AutoNAT v2, magicsock + `/nu/qpath/1` + disco (`/nu/path/1`), portmap, netmon, mDNS, peer sessions, dnsaddr/routing lookup, NAT lab, `nu doctor` | 100 % connect across the NAT matrix (UDP-blocked excepted, with a clear error); Nu↔Nu goes direct wherever the NATs allow; a transfer survives a Wi-Fi → hotspot switch **without reconnecting** |
| **0b+ – Hard NAT** (in parallel with 0c) | NAT profiling (mapping, allocation pattern, filtering, lifetime), port prediction, birthday spraying + guardrails, datagram relay extension, pair-aware relay choice, peer relays, real-hardware lab, opt-in telemetry | Direct for every NAT-lab pair with ≥ 1 EIM/public/mapped/IPv6 side; ≥ 98 % for EIM ↔ random-EDM; per-pair success table published |
| **0c – Share files** | `core`, store, `/nu/bytes|tree|have`, kad (Amino), CLI add/get/cat/ls/pin/share, local gateway | Two laptops behind different NATs share a 10 GB file and a 10k-file tree with no config; checked download from 3 peers |
| **1 – Names** | OAuth, `nu.name`/`nu.node`, publish/resolve/get by handle, proofs, cache | Cold resolve < 1 s; open a published site from another machine |
| **2 – Replication** | follow (Jetstream), my-devices seeds, GC, history | Publish from a laptop, laptop sleeps, a third party fetches from the seed |
| **3 – Reach** | WSS/TCP 443 on relays + Noise/yamux (Q19); DCUtR with standard peers (D11b); public HTTPS/RASL gateway + HTTP mapping of the sync protocols (if needed); browser listeners; IPFS bridge; optional gossipsub | Works on a UDP-blocked network via relay; hole punch with a NAT'd Kubo node; Helia in a browser fetches from a Nu seed |
| **4 – Private + Mode B** | encryption, Spaces for names, self-hosted identity | Encrypted share to a DID; names with no PDS |

## 19. References (checked Sept 2026)

- libp2p specs: QUIC https://github.com/libp2p/specs/blob/master/quic/README.md — TLS
  https://github.com/libp2p/specs/blob/master/tls/tls.md — hole punching
  https://github.com/libp2p/specs/blob/master/connections/hole-punching.md — circuit v2
  https://github.com/libp2p/specs/blob/master/relay/circuit-v2.md — DCUtR
  https://github.com/libp2p/specs/tree/master/relay — AutoNAT v2
  https://github.com/libp2p/specs/blob/master/autonat/autonat-v2.md
- go-libp2p relay defaults (2 min / 128 KiB):
  https://pkg.go.dev/github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay
- DCUtR measurements: IMC '26 / arXiv https://arxiv.org/html/2510.27500 — ProbeLab report
  https://github.com/probe-lab/dcutr-project/blob/main/docs/dcutr-final-report.md — AutoNAT v2
  audit https://github.com/probe-lab/autonat-perf-audit/blob/main/docs/final-report.md
- Ethereum consensus p2p interface: https://ethereum.github.io/consensus-specs/specs/phase0/p2p-interface/
- dnsaddr: https://github.com/multiformats/multiaddr/blob/master/protocols/DNSADDR.md —
  delegated routing v1 (incl. IPIP-417 peers): https://specs.ipfs.tech/routing/http-routing-v1/
- iroh (behavioural model): 1.0 https://www.iroh.computer/blog/v1 — NAT traversal
  https://docs.iroh.computer/concepts/nat-traversal — address lookup
  https://docs.iroh.computer/concepts/address-lookup — QAD https://www.iroh.computer/blog/qad —
  multipath/QNT https://www.iroh.computer/blog/iroh-0-96-0-the-quic-multipaths-to-1-0
- n0 net-tools (`portmapper`, `netwatch`): https://github.com/n0-computer/net-tools
- Tailscale (Go reference for magicsock/disco/DERP/portmapper/netmon): How NAT traversal works
  https://tailscale.com/blog/how-nat-traversal-works — magicsock
  https://pkg.go.dev/tailscale.com/wgengine/magicsock
- quic-go: Transport (shared socket, non-QUIC packets, GSO) https://quic-go.net/docs/quic/transport/
  — multipath status https://github.com/quic-go/quic-go/issues/3343
- go-iroh (shows in-QUIC multipath/QNT in Go needs quic-go + crypto/tls forks): https://github.com/tmc/go-iroh
- indigo (Go atproto): https://github.com/bluesky-social/indigo
- iroh-blobs protocol (bao-tree encoding): https://docs.rs/iroh-blobs/latest/iroh_blobs/protocol/
- Kubo Provide Sweep: https://ipshipyard.com/blog/2025-dht-provide-sweep/
- atproto: sync https://atproto.com/specs/sync — permissions https://atproto.com/specs/permission
  — Spaces https://atproto.com/blog/atproto-spaces-alpha — PLC org
  https://atproto.com/blog/plc-directory-org — jacquard https://github.com/rsform/jacquard
- DASL: https://dasl.ing/ (CID, DRISL, BDASL, RASL)
- IPFS trustless gateway: https://specs.ipfs.tech/http-gateways/trustless-gateway/
