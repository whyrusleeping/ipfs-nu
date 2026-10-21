# Following datasets

[Spec overview](../spec.md) · [Data and replication](data.md) · [Names](names.md) · [Networking](networking.md) · [Clusters](cluster.md)

A **dataset** is an ordinary [name](names.md#d5-records) whose tree target the publisher
replaces over time, such as a Wikipedia mirror, a package archive or a map extract. Following
a dataset pins its current tree and replaces that pin with each newer verified target,
transferring only what changed. ("Dataset" avoids confusion with atproto record collections.)

This extends [D7](names.md#d7-updates-and-following) and
[D17](data.md#d17-store-and-replication) from small personal sites to large, frequently
updated trees. No new record type is needed. A follow is stored as a mirror-preset dataset
with a `name` source in the daemon's [local cluster](cluster.md#d20-clusters), so follows and
datasets share one listing and status model; D19 is the sync engine behind both. A plain
follow tracks current state, not
archival history: it may keep old bytes as a safeguard, but offers no history lookup.
[Archive datasets](cluster.md#version-history), including the simple one-node case, preserve
selected versions explicitly. CID access still works for any old content that remains held.

## D19. Following large, changing trees

### Design target

These are scale-track sizing targets, not measured results or prerequisites for the first
useful mirror/archive pilot. Representative workloads and supported limits belong in
[validation](validation.md):

| Quantity | Target |
|---|---|
| Dataset | 10M entries, 100 GB–1 TB of blobs, entries possibly in one flat directory |
| Publication cadence | one new target every 1–15 minutes |
| Change per publication | 1k–10k changed entries, scattered across the key space |
| Followers | 10k, most online, some offline for days |

At fanout 16, a 10M-entry directory MST is about 6 levels deep. Structure is roughly
1 GB, fetched once. In estimates, 1k scattered changes replace a few thousand MST nodes, or
a few MB of structure per publication, plus the changed blobs. Benchmarks with the
[D3 fanout work](data.md#d3-directory-trees) must confirm these estimates.

Goals: followers stay browsable at a consistent version while syncing. A follower one version
behind transfers roughly the changed blobs plus changed structure. Publication load on the
publisher's PDS does not grow with the follower count.

### Version ordering

A target's version is the `rev` (TID) of the signed repo commit in its proof. Follow state
stores the rev held for each followed name. Proofs relayed by peers are accepted only if
their rev is strictly newer; a proof with the same record CID refreshes the rev without
triggering a sync. Proofs with revs beyond a few minutes of clock drift into the future are
rejected, as in atproto sync. An answer fetched directly from the current PDS is
authoritative under [D6](names.md#d6-resolution-and-caching) and may reset the stored rev,
for example after a repository reset or migration. This ordering exists only so followers
can accept relayed proofs; it does not add the rollback protection that D6 disclaims.

### Publishing

- **Incremental import:** `nu add -r` keeps an import cache of (path, size, mtime, inode,
  ctime) → blob CID and does not rehash unchanged files. MSTs are canonical, so an
  incremental import yields the same root as a fresh import of the same files.
- **Programmatic edits:** `nu tree edit <root> --put <path>=<cid> … --rm <path> …`, and the
  equivalent API call, produce a new root. Only O(changes · log n) nodes are written, so
  publishers generating content from change feeds need no directory on disk.
- **Coalescing:** the daemon publishes a given name at most once per
  `publish.min_interval` (default 60 s), always publishing the latest root. A Bluesky-hosted
  PDS charges 2 points per update against 5,000/hour and 35,000/day per account, which is
  about 17.5k updates/day across all names. Other PDSes have their own limits. Publication
  therefore happens in epochs, not per change.
- **Retained structure:** publisher and seed nodes keep the directory/MST nodes, but not the
  blobs, of the last `follow.retain_structure` targets (default 32). This lets them answer
  server-side diffs for followers who are behind. Shared subtrees make this cheap. It is a
  diff cache, not history: nothing resolves to these roots and GC removes them.
- After a successful write, the publisher daemon fetches its own proof once and pushes it
  into the dataset swarm (below). Seeds follow like any follower. The ingesting machine
  may go offline only after an exact-target wait confirms the chosen seed fully holds the
  requested content; publication/proof receipt alone is not readiness.

### Update notification

Every source below is only a hint that a newer version may exist. A follower acts only on a
verified proof: a signed commit plus the MST inclusion path for the record, as in D6.
Proofs are self-certifying, so any peer can relay them.

1. **Peer push (`/nu/name/1`, primary path):** followers keep `watch` streams to a few
   swarm peers, preferring seeds. A node that accepts a newer proof forwards it to its own
   watchers, flooding a random mesh deduplicated by rev. A publication therefore normally
   costs one PDS request, the publisher's own, however many followers there are.
2. **Jetstream (independent path):** subscribe with `wantedDids` = followed publishers and
   `wantedCollections` = `io.ipfs.nu.name`. Events are unsigned. A newer rev in an event
   triggers a `get` from swarm peers. After a random 0–30 s delay, if no peer has supplied
   the proof, the follower fetches it from the PDS. Resume from the stored Jetstream cursor
   after downtime; past the lookback window, re-resolve instead. Jetstream prevents a set
   of withholding peers from hiding updates.
3. **Poll backstop:** re-resolve every followed name from its PDS on startup, after
   reconnecting, and every `follow.poll` (default 1 h, jittered).

A node that already consumes a signed firehose (relay `subscribeRepos` or a small PDS's own
stream) can build proofs from `#commit` blocks without calling `getRecord`. This mode is for
infrastructure; ordinary followers do not consume the full firehose.

Archive datasets whose source is a whole atproto account (`atrepo`) use the same hints,
subscribing to all of the account's collections. They advance by verified
`getRepo?since=` diffs rather than record proofs ([D21](dasl.md#atproto-accounts-as-archive-datasets)).

```text
→ /nu/name/1
→ {op: "get", did, name, after?: rev}
← {status, rev?}, then a proof CAR: commit, MST path, record (or absence proof); FIN
→ {op: "watch", subs: [{did, name, after?}]}          # long-lived
← proof CAR frames as newer revs are accepted
```

Framing and limits follow [D14](data.md#framing-and-limits). Bound watchers per node (default
256) and subscriptions per watcher. Peers can withhold or delay proofs, but cannot forge them.
A verified deletion or absence ends the follow: the last target stays pinned, and the user
is notified and decides what to remove.

### Dataset swarm

Providing every blob of a 10M-file dataset from every follower is infeasible on Amino.
Instead, followers provide one **swarm key** per followed name, the raw BLAKE3 CID of
`"nu.follow\0" + did + "\0" + name`, plus their current tree root. The swarm key is
announced through Amino and, if configured, delegated routing.

- Swarm peers are candidate providers for every CID in the dataset. They are queried with
  `/nu/have/1`, which also covers peers on slightly older versions that hold most blobs.
  This adds a step to [D15](data.md#d15-content-provider-discovery) for name-context
  fetches.
- The swarm is also the `/nu/name/1` watch mesh.
- `follow.provide = swarm|roots|all` defaults to `swarm` (swarm key + current root). `all`
  restores per-blob announcements so that bare-CID lookups outside the name context find
  followers.
- `--no-announce` follows unannounced. It neither provides the swarm key nor serves
  watchers; this is not privacy/access control from contacted peers or name services.
  Announced follows are publicly discoverable as in the [sharing table](../spec.md#sharing-and-privacy).

### Delta sync

Given a complete held target A and a verified newer target B:

1. **Server-side diff (normal case):** request
   `/nu/tree/1 {root: B, scope: subtree, have: [A]}` from seeds or swarm peers holding B
   and A's retained structure. The server walks both trees and streams only B's new nodes,
   verified on arrival. Split large diffs with `name_range` across peers.
2. **Client-driven diff (fallback):** if no peer retains A, walk B from the root with batched
   node requests (`/nu/tree/1 {nodes: [cid…], scope: node}`). Descend only where a child
   CID is not locally complete. Round trips are roughly the total MST depth along changed
   paths, about 6–10 for the target shape. Batch size is bounded.
3. **Blobs:** for new file entries, fetch blobs not already in the store over `/nu/bytes/1`
   from swarm peers, split across multiple peers as in D2. Small blobs arrive inline.
   Content already present, whether reverted, moved or shared with another dataset, is not
   fetched.
4. **Completeness marks:** the store marks a directory or MST subtree CID complete once all
   of its descendants and blobs are present, propagating bottom-up. Marks make resume
   stateless and retargeting cheap. `have` may only list complete roots.
5. **Atomic held-view switch:** when B is complete for the configured full/scoped/structural
   mode, one DB transaction moves the held follow pin/view from A to B and records B's rev.
   The follow gateway serves B thereafter. The latest verified name cache remains separate
   from held state so `resolve`/name `get` do not conceal lag. Each retrieval binds one CID;
   mutable gateway requests across a switch need immutable links for snapshot consistency.
   A stays pinned as D17's previous-target safeguard until the next switch; then GC may
   collect blobs unique to A. Followers also retain the structure of a few recent targets
   (default 2) so they can answer diffs for peers one version behind.
6. **Retargeting:** if a newer C is verified during a sync to B, the sync switches to C
   immediately. Completed B subtrees shared with C are kept, and only content unique to B
   is wasted. When the publication rate outpaces a follower's bandwidth, `nu follow ls`
   reports the follower as falling behind rather than silently lagging.

Files are diffed at whole-file granularity. A changed blob is a new CID and is fetched in
full, subject to D2 range sharing with other peers. **Later:** for a same-path file change,
compare Bao chaining values at identical offsets with the old blob. Groups that match can be
copied locally, so only differing 16 KiB groups are fetched. This helps appends and in-place
edits, not insertions.

### Follow scopes and quotas

| Option | Effect |
|---|---|
| (default) | Full tree and all blobs pinned |
| `--path <prefix>` | Pin the proof chain plus one subtree (e.g. one language, one namespace) |
| `--lazy` | Pin all structure; fetch blobs on access and cache them unpinned |
| `--select <rules>` | Collections only: pin all structure (the catalog) plus items chosen by local metadata rules ([D22](collections.md#following-collections)) |
| `--max-size <bytes>` | Logical referenced-byte quota for the selected dataset/scope; not cumulative download allowance |

Before syncing B, estimate logical size and additional unique physical bytes/headroom where
known. Publisher fields are claims; verify actual sizes and enforce limits during fetch.
If logical, physical or transfer limits stop progress, preserve A and report a pause plus
required space/budget and next action. Physical limits include old/history/staging/surplus,
not just B. Repeated retargets need a separate transfer budget/bandwidth control.
See [capacity contracts](operations.md#capacity-and-transfer-budgets) for dedup accounting,
free-space reserve and cleanup. Lazy readiness covers structure, not offline blobs; missing
or type-changed scoped paths pause without silently expanding to the full dataset.

### Lifecycle and reader contract

Expose latest observed, syncing and held/served roots, mode, lag, progress and blockers.
`get` of a name binds its resolved root; follow gateways use the held snapshot until its
switch. Full follows become usable after content completion, scoped ones only for their
scope, and lazy ones only structurally. See [reader versions](operations.md#reader-visible-versions).

Verified name deletion ends the follow but leaves visible orphaned retained pins for the
user to manage. Explicit unfollow releases that follow's pins only; other pin sources stay.

The three roots are independent state: *observed* is the newest verified proof's target
(the name cache, possibly newer than anything held); *syncing* is the target the engine is
currently acquiring, if any; *held* is the root the follow gateway serves. Readiness
reports ([E2](operations.md#e-readiness-evidence)) always carry the follow's mode:
`full` is complete when every structure node and blob of the target is verified present;
`scoped:<path>` when the proof chain from the root to `<path>` and the whole subtree are;
`lazy` when all structure is, blobs being cached on access. A scoped follow whose path is
missing or changes type pauses at the previous held root (`paused`, reason shown) and
never widens; a lazy follow is never `full`-complete, so a seed asked for a `full` wait
while following lazily reports `incomplete`, not success.

### Seed waits

A seed is a storage member of the publisher's cluster ([product](product.md#keep-it-available)),
following the name or pinning a fixed CID. `nu publish <name> <cid> --wait-seed <peer>`
and `nu wait --holder <peer> <cid>` apply [W1–W5](operations.md#w-exact-target-waits):

- The wait binds the exact CID the publisher wrote, never the name. It is satisfied only
  by the seed's fresh completion report with `target = <cid>`, `mode = full`,
  `complete: true`, received over `/nu/cluster/1` heartbeats or the authenticated
  `status` stream. Name resolution, proof receipt, `metadata.acknowledged` and the seed
  listing the name in `follow ls` do not satisfy it (operations R2).
- The result has `publication` (rev, record CID, PDS answer) and `wait` ([W3](operations.md#w-exact-target-waits))
  as separate objects; `publish` without `--wait-seed` prints that the content is not
  yet seed-held. A newer publication during the wait sets `superseded_by`; the wait does
  not move (W1).
- After `satisfied: true` the publisher may go offline; `nu follow ls`/`status` on the
  seed then show the dataset as `held` with `confirmed: 1, required: 1`, and a fresh
  third client fetching the CID succeeds from the seed alone (operations R19).
- Seeds in a private/isolated cluster serve members only; public retrieval needs public
  mode and `announce` ([cluster](cluster.md#public-mode)).

### DID-wide follows

`nu follow <did-or-handle>` (no name) follows every `io.ipfs.nu.name` record of the
account as one *DID follow* that owns per-name mirror datasets:

- **Future names.** Names created after the follow are followed automatically with the DID
  follow's options; `nu follow ls` lists them under the DID. `nu unfollow <did>/<name>`
  writes an exclusion so that name is not re-followed; `nu unfollow <did>` removes all.
- **Non-tree targets.** File and blob records are followed as single-blob datasets. The
  held root of each sub-follow is whatever kind its record names; a kind change is a new
  target like any other.
- **Quotas.** `--max-size` on the DID follow is an aggregate logical quota over all its
  names; `--path` and `--lazy` apply per name. When a new or grown name would exceed the
  aggregate, that name pauses with `quota` and the others continue; nothing already held
  is released to make room.
- **Deletion and recreation.** A verified deletion ends that sub-follow and orphans its
  pins visibly (above). A later record with the same name is a new sub-follow and is
  followed unless excluded; its orphaned pins remain separate until the user removes them.
- **Status.** The DID follow reports the worst state among its names (`paused` if any is
  paused) plus per-name observed/syncing/held roots; readiness and waits are per name.

### Scope boundaries

Producing datasets is in scope, sequenced after following. The first producer paths are
URL import and the [collection item push API](collections.md#ingest), which external
producers feed after doing their own source discovery.
Converters for other sources, such as MediaWiki dumps and change feeds, are later producer
work, not excluded. Until they exist, external tools build those datasets with `nu tree edit`
or the API.

Search indexes and bundles are ordinary files. Datasets that rewrite them wholesale each
epoch transfer them in full, so publishers should shard such files. Nu does not reconcile
multiple publishers of the same dataset; only cluster collections merge several writers'
heads ([D22](collections.md#union-heads)). Identical content still shares blobs, and
therefore providers, across datasets.

## Details still to specify

- Logical/physical/transfer accounting ([0064](operations.md#capacity-and-transfer-budgets)).
- Exact `/nu/name/1` messages, proof CAR limits, and watch-mesh degree and forwarding rules.
- Whether firehose `#commit` blocks always contain a complete inclusion path for the record
  (MST inversion suggests so), so they can serve as proofs; verify against indigo.
- Swarm-key encoding and its acceptance by Amino, together with
  [Q2](../spec.md#open-decisions-and-verification).
- Completeness-mark storage cost at 10M entries, and crash-consistency of the atomic switch.
- Defaults for retained structure, poll interval and PDS fallback delay, set from measurements.

## References

- [atproto sync: firehose, `#commit` inversion, validation rules](https://atproto.com/specs/sync)
- [Jetstream design](https://github.com/bluesky-social/jetstream/blob/main/docs/README.md)
- [Bluesky PDS rate limits](https://docs.bsky.app/docs/advanced-guides/rate-limits)
