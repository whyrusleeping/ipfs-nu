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
refers to those collections. Use `indigo` for atproto identity, crypto and repo proofs; the
OAuth client is Nu's own small implementation (below) so that every request runs under
`httpsrc`'s policy and the test fixtures.

### Login and discovery security (0045; moved here from 0035)

`nu login <handle-or-did>` is the atproto OAuth profile for a **public client**: PAR, PKCE
`S256`, DPoP `ES256`, no client secret. The daemon (or the daemonless command) runs the
whole flow; the CLI only opens the browser and waits for the operation.

- **Discovery chain, every step through `httpsrc`.** Handle → DID (DNS TXT, then
  `/.well-known/atproto-did`), DID document (plc.directory or did:web), the PDS from the
  document, `<pds>/.well-known/oauth-protected-resource` → exactly one
  `authorization_servers` origin, `<as>/.well-known/oauth-authorization-server`. Each URL is
  checked before the dial (https, no credentials, no address zones, no private, loopback,
  link-local, special-use or IPv4-mapped destinations; resolved addresses are checked and
  dialed as literals, mixed answers fail closed), bodies are bounded (64 KiB), redirects
  are refused on auth and XRPC requests, responses must be exactly 200 `application/json`.
  The identity used for login is re-resolved when older than a minute (the profile asks
  for short cache lifetimes in auth flows).
- **Malicious endpoint validation.** The resource metadata's authorization server must be
  an origin (https, no path/query/fragment/default port). The server metadata must have
  `issuer` equal to that origin and `authorization_endpoint`, `token_endpoint`,
  `pushed_authorization_request_endpoint` (and `revocation_endpoint` if present) as https
  URLs, support `code`, `authorization_code` + `refresh_token`, `S256`, the `atproto`
  scope, DPoP `ES256`, `token_endpoint_auth_method` `none`, and set
  `authorization_response_iss_parameter_supported`, `require_pushed_authorization_requests`
  and `client_id_metadata_document_supported` to true; `require_request_uri_registration`
  must not be false. Anything else is refused before any request to it.
- **Binding.** The flow is bound to the DID resolved at its start (`login_hint` carries
  what the user typed). The callback must carry the flow's `state` and the bound
  `iss`; the token response must have `sub` equal to that DID, `token_type` `DPoP`,
  a refresh token and a `scope` containing `atproto`, or the session is rejected. Each
  DPoP proof has a fresh `jti`, `htm`, `htu`, `iat`, the server's current `nonce` and, on
  resource requests, `ath`; `use_dpop_nonce` answers are retried once with the new nonce,
  an expired access token is refreshed once. Refresh tokens are single-use, so token use
  is serialized per DID inside the daemon.
- **Client identity.** The default `client_id` is the profile's localhost development
  form, `http://localhost?redirect_uri=http://127.0.0.1/callback&scope=…`, which binds a
  loopback redirect on any port and needs no hosted metadata document; the callback
  listens on `127.0.0.1:<ephemeral>/callback` for one login (10 min). A hosted
  `client_id` can replace it in `atproto.Config.OAuth` once nu.ipfs.io publishes a
  client metadata document (open product question: hosted metadata needs a custom-scheme
  or https redirect, not loopback).
- **Scope.** Nu requests `atproto repo:io.ipfs.nu.name repo:io.ipfs.nu.node` and nothing
  else; a PDS refusing a write outside them is reported as `forbidden`. The
  `io.ipfs.nu.authPublish` permission set replaces the granular scopes once its lexicon
  is published under nu.ipfs.io.
- **Token storage.** Sessions (tokens, DPoP key, nonces) are JSON files, mode 0600, in
  `$NU_PATH/auth/` (mode 0700), one per DID, written atomically. They never appear in the
  API (`/auth/accounts` lists DID, handle, PDS, scopes and times), in bundles or exports.
  `nu logout` revokes the refresh token at the server when it has a revocation endpoint
  and deletes the file either way.
- Named publication requires a login; content commands and name resolution never do.

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
same code ([D21](dasl.md#using-atproto-data)). Package `atproto` implements them
(0053): handles by DNS TXT then `/.well-known/atproto-did`, did:plc through
`plc.directory` and did:web, every HTTPS step through `httpsrc`'s SSRF policy (a PDS
endpoint from a DID document is checked like any URL), identities cached for 10 minutes
with the age exposed on each answer, and one re-resolution after a signature failure.

*Implemented (0045):* `names.Resolver` caches verified answers for 5 minutes per
`<did>/<name>` and per `<handle>/<name>`, so a hit makes no name-plane request at all;
every answer carries `source` (`pds` or `cache`), `age` and `identity_age`. `nu resolve
--fresh` forces a PDS fetch, `--offline` answers only from the cache (any age, labeled),
`--max-age` bounds the accepted age. A publisher's own `publish` fetches its proof once
and primes the cache. Deletions are cached as proven absences (the rev is reported).

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

*Implemented (0045):* followers re-resolve on startup, on `nu follow refresh`, on
every poll (`node.FollowPoll`, 1 h jittered) and, for the publisher's own daemon, at
`publish` time. The Jetstream hint source is opt-in config (`[follow] jetstream`) restricted to the
followed DIDs; peer push (`/nu/name/1`) is not built yet. Both are hints that trigger
the same verified refresh.

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
they are not automatic uploads or a project hosting guarantee. *(0043)* `nu share` prints,
after the `nu://` link, `https://<domain>/tree/<cid>/<path>` (or `/blob/<cid>`), the site
origin `https://<cid>.<domain>/<path>/` for a directory and `rasl://<cid>/?hint=<domain>`
only when the daemon answering runs the [public listener](api.md#public-listener-0043)
and the root is marked public (`nu public add`); a daemonless `share` never prints them. Nu-aware links carry provider
hints; conventional browser links trust that gateway. Followed-name browser links can serve
a held snapshot, so expose its CID and immutable snapshot link. CLI/API default name reads
resolve a target; an explicit held/offline selection is a separate mode.

## References

- [atproto identity](https://atproto.com/specs/did)
- [Repositories](https://atproto.com/specs/repository) and [sync](https://atproto.com/specs/sync)
- [OAuth](https://atproto.com/specs/oauth) and [permissions](https://atproto.com/specs/permission)
- [indigo](https://github.com/bluesky-social/indigo)
- [Spaces (later private names)](https://atproto.com/blog/atproto-spaces-alpha)
