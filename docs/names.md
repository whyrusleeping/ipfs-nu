# Identity and names

[Spec overview](../spec.md) · [Data and replication](data.md) · [Networking](networking.md) · [Following](follow.md) · [Clusters](cluster.md) · [Operations](operations.md)

Nu uses atproto for identity and signed mutable names, not as a file-serving network.

## D4. Identity

- Support `did:plc` and `did:web`; verify handle ↔ DID binding in both directions.
  The DID document supplies the PDS and signing key, independently of node PeerIds.
- **Mode A (default):** use an existing atproto account through OAuth (DPoP, PKCE, PAR).
  The PDS holds the repo signing key and signs commits. Use a Nu permission set,
  `io.ipfs.nu.authPublish`, granting `repo:io.ipfs.nu.name` and `repo:io.ipfs.nu.node`.
- **Mode B (extensions milestone):** the daemon holds the signing key and serves its own single-account
  repository.
- **Cluster state repositories** ([D20](cluster.md#cluster-state)) are byte-exact atproto v3
  repositories ([D21 profile](dasl.md#atproto-repository-profile)) signed by P-256 `did:key`s.
  They are served only inside a cluster, never hosted on a PDS or visible on the firehose, so
  clusters work without a PDS or DID resolution. Stock atproto tools can still inspect and
  verify their CAR exports.
- Content commands (`add`, `get`, `cat`, `pin`, `share`) require no identity.

The namespace is `io.ipfs.nu`, from **nu.ipfs.io**. The shorthand `nu.name` / `nu.node` below
refers to those collections. Use `indigo` for atproto identity, crypto, OAuth and repo proofs.

## D5. Records

```text
nu.name / <name>
{ target: "<cid>", kind: "tree"|"file"|"blob", size?, note?, updatedAt }

nu.node / <peerId>                  # rkey: base32 CIDv1 libp2p-key form
{ label?, roles: ["device"|"seed"],
  relays?: ["/dns4/relay1…/udp/443/quic-v1/p2p/<relay>/p2p-circuit", …],
  dnsaddr?: "/dnsaddr/<peerid>.peers.nu.ipfs.io",
  createdAt }
```

Name rkeys are flat; no aliases in v1. Targets are strings, so the PDS need not accept
BLAKE3 blob CIDs as repository links, and one field holds every kind. Node records bind a
DID to PeerIds and contain relay addresses and a dnsaddr name, never raw IP addresses.

A name is `at://<did>/io.ipfs.nu.name/<name>`. Nu resolves its current target. There is no
history command, revision-addressed lookup or archive of past signed commits. Opt-in history
belongs to archive datasets ([D20](cluster.md#version-history)), including one-node archives,
not to names. `file` identifies D3a's default standalone wrapper, `blob` its raw bytes.
Publication and content readiness are separate; a successful name write does not tell the
publisher that a seed holds its bytes or that the publisher may sleep.

## D6. Resolution and caching

1. Resolve and verify handle → DID, if the input is a handle.
2. Resolve the DID document for the current PDS and signing key.
3. Fetch `com.atproto.sync.getRecord` from that PDS over HTTPS.
4. Verify the proof CAR's commit signature and record inclusion; obtain the target CID.
5. Fetch content through the data plane, using the publisher's nodes as provider hints.

Steps 1–4 are Nu's generic verified record fetch. `nu get at://<did>/<collection>/<rkey>`
uses them for any collection, and repo snapshots and atproto archive datasets build on the
same code ([D21](dasl.md#using-atproto-data)).

The current PDS is the source for current repository state. Proofs establish authenticity,
not freshness. Cached answers may be stale, including during outages; this is acceptable.
A cache hit needs no name-plane requests. Cold resolution takes roughly 3–4 HTTPS round
trips; the latency target is in the [milestones](../spec.md#milestones).

Nu does not promise rollback prevention or maintain a durable revision high-water mark for
that purpose. Revisions may be cache/update metadata, not user-visible history. Name records
need no periodic expiry renewal. CID fetching remains independent of PDS availability.
Expose cache age/freshness and the selected CID. Name resolution caches and a follower's
held/served view are independent: `get` binds one resolved target, while a mirror gateway
may serve an older complete snapshot. Historical archive verification needs retained
identity/signing-key provenance, not only the current DID document; specify this before
claiming retained proofs survive account/key changes. See [operations](operations.md).

## D7. Updates and following

Jetstream subscriptions (`wantedDids`, `wantedCollections`) trigger a re-fetch of current
state from the PDS **with proof**. The full signed firehose is an alternative. Notifications
prompt refreshes; missed updates can leave cached state stale. Followers pin resolved targets
under the [replication policy](data.md#d17-store-and-replication).

Because proofs are self-certifying, followers of a name can also relay them to each other
over `/nu/name/1`, accepting only newer revs. Jetstream and polling remain as independent
paths. This, plus delta sync of large changing trees, is specified in
[D19](follow.md#d19-following-large-changing-trees).

## Links

```text
nu://alice.example.com/site/docs/intro.html
nu://did:plc:abc…/site
nu://bafyrei…/docs/intro.html
nu://bafyrei…?peer=<peerid>&addr=<multiaddr>…&via=did:plc:abc…
rasl://bafkr4i…/?hint=host.example
at://alice.example.com/app.bsky.feed.post/3k…        # any atproto record (D21)
https://seed.example/tree/<cid>/docs/intro.html
```

`nu share` prints the shortest link that works for the recipient: the CID plus the sharer's
base36 PeerId, adding `addr=` multiaddrs (direct or relay circuit) only when no lookup source
(dnsaddr, routing v1, mDNS) can supply them. Links are meant to be pasted into chat, so
hints stay few and the CID comes first.

**Grammar (v0, `node/link`).**

```text
nu://<cid>[/<path>][?peer=<base36 peerid>[&addr=<multiaddr>]…[&via=<did>]]
```

- `<cid>` is a canonical DASL CID (`core/cid`: raw or DRISL, SHA-256 or BLAKE3, lowercase
  base32). `<path>` is a slash path under it; segments are percent-encoded where needed and
  may not be empty, `.`, `..` or contain NUL; no trailing slash.
- `peer=` names a holder. The `addr=` values that follow it, until the next `peer=`, are
  that peer's transport multiaddrs (`/ip4/…/udp/…/quic-v1`, `/dns4/…`, relay circuits
  later) **without** a `/p2p/` component; an `addr=` before any `peer=` is an error. Any
  legacy or multibase PeerId text parses; links are written with base36 (`k51…`), which is
  case-insensitive and safe in chat and subdomains.
- `via=` is a DID whose atproto record could supply the peer's current addresses; v0
  carries it but does not resolve it.
- Bounds: whole link ≤ 2048 bytes, ≤ 8 peers, ≤ 8 addresses per peer, each address ≤ 256
  bytes, path ≤ 1024 bytes. Unknown query keys, duplicate peers, fragments and whitespace
  are rejected. Formatting a parsed link reproduces it byte for byte (a fuzz target checks
  this).
- **What the sharer includes (v0).** There is no dnsaddr, routing or mDNS lookup yet, so
  `share` and `add` always include the node's PeerId and up to 3 transport addresses: the
  running host's advertised addresses, or, without a host, the `swarm.listen` config with
  `0.0.0.0`/`::` expanded over the machine's non-loopback, non-link-local interface
  addresses (all IPv4, global IPv6). Once lookup sources exist, `addr=` is added only when
  none of them can supply the address.

HTTPS share links require an explicitly serving public seed/gateway with configured TLS;
they are not automatic uploads or a project hosting guarantee. Nu-aware links carry provider
hints; conventional browser links trust that gateway. Followed-name browser links can serve
a held snapshot, so expose its CID and immutable snapshot link. CLI/API default name reads
resolve a target; an explicit held/offline selection is a separate mode.

## References

- [atproto identity](https://atproto.com/specs/did)
- [Repositories](https://atproto.com/specs/repository) and [sync](https://atproto.com/specs/sync)
- [OAuth](https://atproto.com/specs/oauth) and [permissions](https://atproto.com/specs/permission)
- [indigo](https://github.com/bluesky-social/indigo)
- [Spaces (later private names)](https://atproto.com/blog/atproto-spaces-alpha)
