# Operational contracts

[Spec overview](../spec.md) · [Product brief](product.md) · [Following](follow.md) · [Clusters](cluster.md)

These are product requirements, not claims of implemented guarantees. Owning protocol docs
must specify and test the evidence, crash behavior and authorization that realize them.
Unsettled mechanisms below are feature/release blockers, not invitations to report success.

## Readiness and status

Separate local import, signed-state acknowledgment, public publication, verified content
completion, and policy readiness. A successful name/state write proves none of the later
steps. Every operation returns the exact target/version and the stage reached.

| State/dimension | Meaning |
|---|---|
| Accepted | Local operation/state write accepted; response states who acknowledged metadata |
| Published | Public name write completed; record identifies its target |
| Observed | An authenticated target/version is known; content may be absent |
| Syncing | Fetching/verifying a target while preserving the held usable version |
| Held/served | Locally usable snapshot for the declared full/scoped/structural mode |
| Ready/replicated | Fresh content confirmations satisfy the requested seed or cluster policy |
| Degraded | Previously held/ready content no longer has enough known available copies |
| Unknown | Evidence is stale or inaccessible; do not infer either readiness or data loss |
| Paused/error | Progress stopped, with reason, retained version and recovery action |

These are dimensions, not a single linear lifecycle: published content can still be syncing;
a replicated snapshot can later become unknown or degraded.

Readiness is conditional on trusted members' reports, policy feasibility and current
evidence. It is not consensus, indefinite availability, or proof of independent physical
storage. The rules below are numbered so tests and reviews can cite them; the report
encoding and the placement-side release algorithm are in [cluster.md](cluster.md#status),
seed waits and follow modes in [follow.md](follow.md#seed-waits).

### E. Readiness evidence

- **E1 Exact target.** Evidence names one exact target: a CID, plus for archives the
  version-tree root of the retention live set. Names, "latest" and heads are never targets.
- **E2 Completion report.** The unit of evidence is a holder's *completion report*
  `{target, mode, policy, epoch, complete, missing?, seq}` ([encoding](cluster.md#status)).
  `complete: true` asserts a verified-complete walk: every structure node of `target` and
  every blob the holder is responsible for under `mode` (`full`: all; `scoped`: the proof
  chain and the subtree; `lazy`: structure only; `owned`: the blobs it owns in `epoch`) is
  present and verified against its CID/outboard, as recorded by completeness marks. A
  report is authenticated by the holder's node key (heartbeat signature) or by the
  authenticated stream it arrives on, and is attributed to that PeerId only.
- **E3 Binding.** A report counts only for the exact `(target, mode, policy, epoch)` it
  names. A report for another epoch, policy or mode is not evidence for the current one.
  `policy` is the hash of the dataset's policy fields; `epoch` is the placement epoch; the
  local cluster uses the constant epoch `local`.
- **E4 Freshness.** A report is *fresh* for `evidence.fresh` (default 5 min) after the
  receiver obtained it over a live authenticated connection, measured on the receiver's
  clock; the report's own `time` is informational. A newer `seq` from the same holder
  replaces the older report, including a `complete: false` replacing `true`. Evidence
  older than `evidence.fresh` is *stale* and counts for nothing: neither readiness nor loss.
- **E5 Feasibility.** Readiness first requires a feasible policy in the current epoch:
  at least `replicas` eligible `active` storage members, and for `spread`, at least
  `replicas` distinct values of the spread tag among them. Infeasible policy reports
  `policy_unsatisfiable` with the shortfall (members or domains missing); it is never
  "ready" and never weakened automatically, including during setup and after failures.
- **E6 Ready.** A dataset is *ready* at target T in epoch E when E5 holds and every
  member that owns at least one blob of T in E (the *owner set*) has a fresh report
  `complete: true` bound to `(T, owned, policy, E)` (a seed's `held` condition uses the
  follow's own mode instead). The owner set is computed locally
  from merged state and placement; for `replicas: all` it is every eligible member. For
  the one-node local cluster the owner set is `{self}` and the local completeness mark
  is the report, so one-node readiness is local completeness at the explicit `k=1`.
- **E7 Counting.** Status reports `confirmed` (owners with fresh complete reports),
  `required` (owner-set size), `missing` (owners without fresh complete reports, each as
  `stale`, `incomplete`, `epoch` (report bound to another epoch/policy) or `absent`),
  `extra` (fresh complete reports from non-owners:
  surplus, draining members, member-selected collection holdings) and `evidence_age`
  (the oldest fresh report counted). `extra` never contributes to `ready`.
- **E8 Transitions.** Once ready, a target becomes *unknown* when some owner's evidence
  goes stale and nothing fresh contradicts it, and *degraded* when fresh evidence or a
  merged-state change shows fewer confirmed owners than required: an owner reports
  `complete: false` or omits the target, or an epoch change (a member `unavailable`,
  `draining` or `removed`) replaces owners that have not yet confirmed in the new epoch.
  Status keeps the last ready `{epoch, evidence_age}` beside the current result.
  Unknown is not degraded; neither is loss. Unknown longer than `evidence.unknown_alert`
  (default 30 min) is an alert condition. Archive-wide readiness is the same rule over
  the live-set root; a new incomplete version makes the archive not ready without
  changing the availability result already established for an older complete version.
- **E9 Audits.** A sampled audit shows that a member served sampled bytes in time. It is
  not a completion report, never raises `confirmed`, and a failed audit lowers nothing
  by itself; it is reported separately and feeds repair policy.
- **E10 Metadata is not content.** Signed-state acknowledgment (`metadata.acknowledged`)
  and content readiness are separate results with separate fields; no response may fold
  them into one success. A head write returns `metadata` and `content.state` separately.

### W. Exact-target waits

- **W1 Binding.** A wait names an exact target (CID, or `<ds>@<tid>`) and one condition.
  The result echoes the target. A head change during the wait never retargets it: the
  wait continues for the original target and the result carries `superseded_by` when the
  head moved. Waiting for the newest head is a separate command, not a mode of `wait`.
- **W2 Conditions.** `metadata` (signed state held by ≥ n storage members, existing
  `MetadataState`), `held` (every named `--holder <peer>` has a fresh complete report for
  the target in the requested mode), `ready` (E6) and `archive-ready` (E6 over the live
  set). `held` with no holder is a usage error. A condition is satisfied only by evidence
  of its own kind: metadata acknowledgments never satisfy `held`/`ready`, and content
  reports never satisfy `metadata`.
- **W3 Result.** `{target, condition, satisfied, mode, epoch, policy, confirmed:
  [{peer, seq, age}], required, missing: [{peer, reason}], extra, evidence_age,
  unmet: [reasons], superseded_by?}`. Exit `0` only when `satisfied`; `timeout` (6) when
  the timeout expires with the condition unmet and the latest snapshot in `details`;
  `failed` immediately when the condition cannot be met without a state change
  (`policy_unsatisfiable`, target not resolvable, holder not a member). Default timeouts:
  `metadata` 30 s, content conditions 10 min; `--timeout 0` waits indefinitely.
- **W4 Seed wait.** "The seed holds it" means the seed's fresh report is `complete: true`
  in mode `full` for exactly the published CID (W2 `held`), not that the seed received the
  name, the proof or the head record. `nu publish --wait-seed <peer>` is `publish` then
  this wait; the publication result and the wait result are separate objects.
- **W5 After success.** A satisfied `held`/`ready` wait is permission for the waiting
  machine to go offline (sleep, disconnect, stop its daemon) without reducing the
  confirmed copies. It is not permission to delete the local copy (`pin rm` and dataset
  removal stay explicit), and it describes that moment: later failures are reported as
  E8 transitions, not retroactive wait failures.
- **W6 Snapshot.** A wait is the same evaluation as `status` at that instant; `status`
  must show every field a wait reports, so an operator can see why a wait is blocked.

### S. Safe release

Protected content is: the content of the current served head, of the last policy-ready
head, of labeled and retention-selected versions, and `cluster-surplus` pins. Release is
per blob, decided locally by the member holding it, and only under all of the following.

- **S1 Candidate.** A blob is a candidate only when the member's merged state no longer
  obliges it to hold it: it is surplus (not owned in the current epoch), or unique to a
  version that is neither current, last policy-ready, labeled nor retention-selected.
- **S2 Evidence.** The member holds fresh (E4) `complete: true` reports from **every**
  owner of the blob in the current epoch, bound (E3) to the current head (and live-set
  root), policy and epoch. "Every owner", not a quorum: a partition that hides one owner
  defers release. With `replicas: k`, fewer than `k` fresh confirmed owners never permits
  release; the releasing member's own copy does not count as one of them.
- **S3 Settled state.** The head, policy and epoch the evidence binds to have been the
  member's merged current state for `release.settle` (default 10 min), no record is held
  back for clock skew, and no owner's fresh report names a different epoch or policy
  (concurrent epochs defer). An owner whose member record changed state within
  `release.settle` does not count.
- **S4 Concurrent heads.** While per-signer heads disagree and the displaced head's HLC
  is within `release.settle`, the content of every disagreeing head is protected. The
  write outcome shows the resulting head and the displaced intent; archive retention
  decides whether the displaced head stays an obligation after settling.
- **S5 First-held protection.** A member that has never held a policy-ready version of a
  dataset protects the last version it fully held, regardless of head changes, and stages
  at most `staging.max_heads` (default 2) incoming heads; further heads are observed only.
- **S6 Drain and handoff.** A `draining` member owns nothing; its blobs become candidates
  and leave under S1–S3. `member rm` refuses while any surplus blob lacks S2 evidence
  unless `--force`, which is destructive, requires the dry-run report and records the
  unconfirmed blobs in the result.
- **S7 Pressure.** Disk pressure never releases protected content; it pauses acquisition
  with the bytes needed and the action. Operators may release with an explicit destructive
  command that prints the S2 shortfall per blob.
- **S8 Pilot default.** `release.auto` defaults to `false`: candidates are reported
  (`nu cluster dataset gc --dry-run`: blob, reason, S2/S3 evidence or shortfall) but not
  released, until 0048's evidence tests (rows R10–R18) pass. Collections with
  `replicas: 0` are outside S (no guaranteed copies to protect).

### Negative-test matrix (readiness)

"Gate" rows are required by the named task before it ships; the harness is
[nutest](../nutest) with a fake clock and partition control.

| # | Rule | Scenario | Expected | Coverage |
|---|---|---|---|---|
| R1 | E10/W2 | head write acknowledged by all storage members, no content fetched | `metadata.acknowledged`, `content.state` not ready; `wait ready` times out | gate: 0046 |
| R2 | E2/W4 | seed holds the name record and structure but not all blobs | `held` wait unmet, missing lists the seed as `incomplete` | gate: 0045 |
| R3 | E1/W1 | head moves from T1 to T2 during `wait T1` | wait keeps T1, `superseded_by: T2`; never succeeds on T2's evidence | gate: 0046 |
| R4 | E3 | member reports complete for T in epoch E1; E2 current | not counted; status `missing` with reason `epoch` | gate: 0046 |
| R5 | E4 | clock jumps past `evidence.fresh` with no new reports | ready → unknown, no degraded, no release | gate: 0046 |
| R6 | E5 | `replicas: 2`, one storage member (or `spread: zone`, one zone) | `policy_unsatisfiable`, shortfall named; `wait ready` fails fast | gate: 0046 |
| R7 | E6/E7 | all owners complete but one surplus holder incomplete | ready; `extra` counts it, `confirmed` does not change | gate: 0046 |
| R8 | E7 | collection holdings or surplus complete, owners not | not ready; `extra` shown, no green status | gate: 0046 |
| R9 | E6 | one-node local cluster, content complete | ready at `k=1`, `required: 1`, never claims two copies | gate: 0047 |
| R10 | S2 | partition hides one owner after a head update | old head's unique blobs stay pinned on every member | gate: 0048 |
| R11 | S2 | `k=2`, one owner complete, the other stale | no release; dry-run names the stale owner | gate: 0048 |
| R12 | S3 | epoch changes (member add) within `release.settle` of readiness | release deferred until settled; then proceeds | gate: 0048 |
| R13 | S3 | two owners report different epochs | release deferred; status shows epoch disagreement | gate: 0048 |
| R14 | S4 | two writers write heads concurrently | both heads' content protected within settle; displaced intent in both results | gate: 0048 |
| R15 | S5 | dataset never ready, heads advance five times | last fully held view pinned; at most 2 staged heads | gate: 0046 |
| R16 | S6 | drain a member while an owner is partitioned | `member rm` refuses, lists unconfirmed surplus; `--force` reports them | gate: 0048 |
| R17 | S7 | disk full during update | acquisition paused with bytes needed; protected pins untouched | gate: 0046 |
| R18 | E9 | sampled audit passes on a member with `complete: false` | not confirmed; audit shown separately | gate: 0048 |
| R19 | W5 | publisher stops after a satisfied seed wait; fresh client fetches the CID | success from the seed alone | gate: 0045 |
| R20 | E8 | an owner's member record set `unavailable` by repair authority | degraded, not unknown; repair starts only with authority online | gate: 0048 |

### Feature-entry gates (readiness)

- **0045 (names and seeds)**: E2 full/scoped/lazy report semantics for follows, W1–W5,
  `publish` and seed wait as separate results, R2 and R19; no claim that publication,
  proof receipt or metadata acknowledgment is readiness.
- **0046 (placement and readiness)**: E1–E8, E10, W1–W3, W6, S5, S7 and `release.auto`
  defaulting to false; R1, R3–R8, R15, R17; `ContentState` carries the W3 fields; one-node
  `k=1` status (R9) with 0047.
- **0047 (presets and history)**: E8 archive-wide readiness over the live-set root, R9,
  and version availability reported separately from readiness.
- **0048 (repair, handoff, audits)**: S1–S4, S6, S8 enabling `release.auto`; E9; R10–R14,
  R16, R18, R20 passing in nutest with partitions and a fake clock before automatic
  release ships. Until then every release is dry-run only.

## Reader-visible versions

Expose latest observed, syncing, and held/served roots independently. Ordinary resolution
uses the verified current-state cache (possibly stale); it is not overwritten with a held
root merely to conceal mirror lag. `get` of a name binds to one resolved root for the whole
operation. A followed-name gateway serves its held snapshot until an atomic switch, clearly identifying
it as the held view. Non-followed names use resolved targets. CLI/API offer explicit held/
offline selection for an existing follow, failing if that scope/content is not locally held;
this differs deliberately from default latest-resolved retrieval. Final flag syntax is open.
Return the selected CID in API/HTTP metadata and offer immutable CID-bound links for
multi-file readers; switching between requests to a mutable URL is not page-wide isolation.

Full follows are usable/offline only after all requested content is verified. Scoped follows
cover the proof chain and selected subtree, not the whole dataset. Lazy follows become
structurally usable once their structure is complete; missing blobs still need a provider.
Status/readiness always includes this mode. A missing or type-changed scope pauses the
follow with the previous held view intact rather than silently broadening the scope.
Retargeting favors newest state, but status must expose lack of progress; a bounded-progress
option remains a decision to validate against pilot workloads.

## Retention and historical availability

`mirror` retains current usable content and an update safeguard. `archive` requires an
explicit retention policy and versioned access. History records and content availability
are separate: observed, fetching, retained-complete, degraded/unknown, skipped/never-completed,
or metadata-only/pruned. Current retention selection is reevaluated as heads arrive: only
still-selected or explicitly labeled incoming versions remain acquisition obligations.
A version not selected for retention may be skipped; observing it does not preserve it.
External-name history records versions actually observed, not every publication ever made.

Immediate retrieval/rollback applies only to available retained-complete versions. Writing a
rollback head and restoring the cluster's requested readiness are separate outcomes. Labels
retain content as well as metadata; budget pressure pauses and warns rather than implicitly
pruning labels, the current served version or the last policy-ready safeguard. Without an
initial policy-ready version, preserve the last fully held view and pause rather than stage
an unbounded succession of heads. Explicit one-node preset means one-copy readiness, not
silently reducing a chosen two-copy policy. Labeling missing content requires explicit
reacquisition intent and reports pending/unavailable status.

Version identity/content/provenance fields cannot change through annotation LWW. Specify
immutable-field validation, collision reporting and annotation authority before implementing
history; administrative pruning remains allowed under the stated retention policy.

Removal, retention changes and pruning support dry-run impact reports and explicit destructive
intent. Report snapshots/content becoming eligible, grace periods and cross-dataset references.
Unfollowing releases only that follow's pins; explicit/other-source pins remain. Deletion of a
public name does not delete local data. Orphaned retained pins are visible and removable.

Retention is not an append-only/compliance guarantee. Explain who may change it, the schedule's
time basis and what survives after pruning. Preserve provenance for retained external versions
and specify the signing-key/identity evidence needed to verify them after account/key changes;
today's DID document alone is not a timeless verification record.

## Capacity and transfer budgets

Three controls, kept distinct; the store's measures, attribution, admission and pressure
mechanics are [D17 A1–A8](data.md#accounting-and-budgets).

- **C1 Logical quota.** `max_size`/`--max-size` bound the logical bytes (A1) of one
  dataset/scope: what it references, counted once per scope, not a cumulative network
  allowance. A new or grown target that would exceed it pauses that scope with `quota`
  before fetching; what is held stays held.
- **C2 Physical budget.** `store.limit` and `store.free_reserve` bound the daemon-wide
  physical bytes (A5): unique blob bytes plus outboards, structure/DB, retained versions,
  staging/partials and surplus, each charged once. Per-scope `unique`/`shared` (A4) say
  what a release would free. A member's `store_limit` in cluster state is this budget.
- **C3 Transfer budget.** T1–T4 below: received bytes over an interval plus bandwidth and
  concurrency caps. Repeated retargeting consumes it even while every version fits C1.
- **C4 Preflight.** Before syncing a target, report the logical size, the additional unique
  physical bytes where known (missing objects by declared size, marked as an estimate),
  staging headroom and policy/capacity shortfalls. Publisher and peer size fields are
  claims (A2): count actual bytes, stop at enforced limits, and never trust a signed size
  as correct. A false or huge claim is refused before any byte is fetched when it cannot
  fit (A6) and disproven by verifying the last chunk group otherwise.
- **C5 Pressure.** On any shortfall: pause new acquisition with the bytes needed and the
  action to resume (`insufficient_storage` for operations; `paused` with the same details
  for follows/datasets), reclaim unpinned cache and abandoned staging (A7), and preserve
  every pin: held, retained, surplus and protected ([S7](#s-safe-release)). Never remove
  archives or retained versions to make a new update fit. Status shows `pressure`,
  headroom and the per-scope attribution so an operator can choose what to release.

### T. Transfer budgets

- **T1 Measure.** Transfer bytes are bytes received from peers (or HTTPS sources), counted
  as they arrive, including bytes later rejected as corrupt, within a window of
  `transfer.interval` (default 24 h) that starts at the first charge and resets when it
  has elapsed. Budgets exist daemon-wide (`transfer.limit`), per
  follow/dataset (`--transfer-limit`) and per peer (serving-side upload caps); each is a
  `store.Meter` with `{limit, interval, used, resets_at}`. Bandwidth (`transfer.rate`)
  and concurrency (`fetch.blobs`, `fetch.peer_requests`) caps shape, not stop, transfer.
- **T2 At the limit.** The consumer pauses the acquisition that would exceed the budget
  with status `paused: transfer_budget {used, limit, interval, resets_at, scope}` and the
  resume action (wait for `resets_at`, or raise the budget). A pause changes no held state:
  the previous held version stays served, partial blobs keep their verified groups and
  resume, pins are not released. A target whose fetch paused is never reported held,
  complete or ready; there is no silent partial "held".
- **T3 Retargets.** Every retarget charges the same budgets. An exhausted budget pauses at
  the next retarget with the held version intact; status shows the lack of progress and
  the next reset.
- **T4 Enforcement.** The meter is the hook; the fetcher charges it when configured and
  the follow/cluster consumers configure it per scope. Until the first consumer ships,
  no transfer budget is enforced and status says so.

Archive cost includes whole changed files initially. Benchmark monolithic rewrites, appends,
small-object changes and sharded indexes before committing update-cost claims. Sub-file reuse
may be promoted by that evidence; structural delta sync is not arbitrary binary delta sync.

## Authority, administration and recovery

Trusted-member access with OS/disk protections is the initial boundary. Members can read
cluster data; writer roles limit state mutation, not read access. Public serving is explicit.
Invite documents carry public verification keys, never private root/admin keys.

Cluster root/admin/node keys and signed state need documented backup/restore procedures,
separate from content export. Warn that `--nocopy` source files are not independent archive
copies and that moving/changing them can invalidate availability.

The fixed genesis root cannot be rotated in place in the current design. Root loss or
compromise needs a documented recovery/re-genesis procedure, authority/provenance checks
and data migration; never suggest that a new admin record replaces the root. Revocation,
node replacement and join credential handling require security review.

LWW is not a transactional editing API. Show the resulting head and displaced intent;
preserve concurrent snapshots when archive retention requests them. Guarded writes can
check the observed head, but cannot promise global compare-and-swap without an additional
protocol. Define detection/reconciliation and explicit conflict results before shipping.

Required operator journeys: initialize/join/tag, update/wait, fix a full member, explain
impossible spread, drain/replace, detect missing repair authority, restore keys/state,
recover onto clean machines, and reconcile concurrent changes. Offer stable JSON output,
exit categories, actionable status and alert conditions. Automatic repair unavailable is
an alert, not a silent operating mode.

## API, gateway and public exposure

The local API, the loopback gateway and any later public/member HTTPS serving share one
security contract. Rules are numbered so tests and reviews can cite them; mechanics and
the current implementation state are in [api.md](api.md). Everything here is a feature
entry gate, not post-release hardening.

### Threat model

Attackers: **T1** any web page the user's browser visits (cross-site requests, DNS
rebinding of a public name to 127.0.0.1, navigations, forms, preflight-free fetches);
**T2** content fetched into the node and rendered by the gateway (HTML/JS, SVG, hostile
MIME types, names, symlinks, MASL headers), including content the user did not choose to
view; **T3** hosts on the LAN/Internet that reach an exposed listener (a port forward or
a `0.0.0.0` listener on a members-only node); **T4** a tree extracted to disk by `nu get -o`.
Assets: daemon control (`add`/`get` read and write local files as the user), the store
contents of a members-only node, other content's browser state, the user's filesystem.
Out of scope: processes running as the same OS user (they are the CLI's trust level),
browser bugs, a compromised OS. The invariant the matrix tests: **a fetched page or tree
can never operate the daemon, read another origin's content or write outside the requested
destination.**

### A. Local API

- **A1** API and gateway listen on loopback by default; a non-loopback `api.listen` or
  `gateway.listen` is refused until the authenticated mode (A8) exists in the build.
- **A2** `Host` must be `localhost`, `127.0.0.1`, `[::1]` or another loopback literal with
  this listener's port, else `403 forbidden`. A public name rebound to 127.0.0.1 still
  carries the public name and is refused.
- **A3** `GET`/`HEAD` never change state. Every state change is `POST`/`DELETE`.
- **A4** For any method other than `GET`/`HEAD`: an `Origin` header must equal
  `http://<Host>`; a `Sec-Fetch-Site` header must be `same-origin` or `none`; `OPTIONS`
  is always `403`. The gateway's origins are foreign to the API and are refused too.
- **A5** Request bodies are read only with `Content-Type: application/json`
  (`400 invalid_argument` otherwise). HTML forms cannot send that type and a script that
  sets it triggers a preflight, which A4 refuses, so browsers that send neither `Origin`
  nor `Sec-Fetch-Site` still cannot deliver a state-changing body. The only exceptions are
  the item push routes' `application/x-ndjson` (`items:batch`) and
  `application/octet-stream` (`uploads`) bodies, which are equally not CORS-safelisted and
  are refused under any other type.
- **A6** No `Access-Control-*` header is ever sent; no cookie is ever set, read or used
  for authorization; responses carry `X-Content-Type-Options: nosniff` and
  `Cache-Control: no-store`.
- **A7** Errors are the JSON envelope; request content is never reflected as HTML.
- **A8** *Authenticated non-loopback API (not in v0; implemented by the task that first
  needs it).* Authorization is a bearer token in the `Authorization` header only (never a
  query parameter or cookie), stored 0600 under `$NU_PATH`, created at init, rotated by a
  command and revoked by deleting it. The token must travel over TLS or a network the
  operator declares trusted in config; A2 applies with a configured host allowlist in
  place of loopback; A3–A7 apply unchanged, so a token never authorizes a
  browser-originated cross-site request. The API listener is never the content listener
  (P1). *State (0059):* implemented only as the **producer listener**
  (`api.producer.listen`, [api.md](api.md#producer-listener-and-push-tokens)), which serves
  the item push routes alone. Its tokens are scoped to named collections, so they are
  created per producer by `nu api token create` rather than at init; `rotate` and `rm`
  apply on the next request. There is no TLS in this build: the daemon refuses to start
  the listener unless `api.producer.trusted_network = true`, and `api.producer.hosts`
  replaces the loopback check of A2. `api.listen` itself stays loopback-only (A1).

### G. Gateway and browsers

- **G1** `Host` must be `localhost`, a loopback literal, or `<cid>.localhost` (exactly one
  label that parses as a canonical DASL CID), with this listener's port; else `403`.
- **G2** Only `GET`/`HEAD` (`400` otherwise); no `OPTIONS` handling, no `Access-Control-*`
  headers, no cookies set, and request cookies are dropped before routing.
- **G3** **Two origin classes.** The *bare* origins (`http://localhost:<port>`, loopback
  IP literals) serve `/tree/<cid>/<path>` and `/blob/<cid>` as **non-active** content only.
  The *site* origin `http://<cid>.localhost:<port>/` serves exactly one root as a site
  (`index.html`, listings, active content allowed) and nothing else: no `/tree`, `/blob`,
  `/api` or other roots exist there. Isolation comes from the browser's same-origin policy
  between distinct hosts, not from the gateway inspecting content.
- **G4** **Non-active fallback.** On a bare origin a file is served inline only when its
  sanitized type is `text/plain`, `text/css`, `application/json`,
  `application/octet-stream`, `image/*` except SVG, `video/*`, `audio/*` or `font/*`.
  Everything else (HTML, SVG, XML, JavaScript, PDF, unknown) is `application/octet-stream`
  with `Content-Disposition: attachment` and a `Link: <site URL>; rel="alternate"`. The
  fallback is the default for every browser; a browser that does not resolve `*.localhost`
  simply never reaches a site origin. Nothing active is ever rendered on a bare origin,
  on an IP-literal origin, or on any path route.
- **G5** **Mutable-name origins** (later `/nu/<handle>/<name>/…`, `/c/<dataset>…`): active
  rendering only on a dedicated origin per name, `<label>.localhost`, whose label is a
  deterministic encoding of the name that cannot parse as a CID (prefix reserved before
  the first name route ships). The name origin re-resolves on each request; it never
  shares an origin with a CID site. Name paths on bare origins follow G4. `/at/…` content
  is inert (JSON/plain text with the listing CSP, blobs as downloads) and has no site origin.
- **G6** **Redirects** are only same-origin path redirects generated by the gateway
  (directory → trailing slash, cleaned path). A request path with an empty segment (`//`)
  is `400`, never redirected. No `Location` ever comes from content.
- **G7** **Types.** Content types are parsed; only a `utf-8` charset parameter survives;
  invalid or missing types fall back to the name's extension, then
  `application/octet-stream`. Blobs are always downloads. `nosniff` is on every response.
  Symlink entries are listed but answer `422`.
- **G8** Gateway-generated pages (root, listings) carry
  `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`; errors are
  plain text, never HTML. Listings link only safe names; unsafe names are shown unlinked. Paths with
  `.`, `..`, empty or NUL segments are `400`.
- **G9** Caching: CID-addressed responses are `public, max-age=31536000, immutable` with
  `ETag: "<cid>"`; errors are `no-store`; name routes (G5) are `no-cache` and revalidate.
- **G10** **MASL headers** (reflected once 0055 lands) pass an allowlist: `content-type`
  through G7 and G4; `content-language` as a language tag; `content-disposition` as
  `inline`/`attachment` with a sanitized filename only; `content-encoding` only when the
  stored bytes are actually encoded and only on site origins. A content
  `content-security-policy` is sent as an *additional* CSP header (browsers intersect
  policies, so it can only tighten). Header values with CR/LF are dropped. `location`,
  `set-cookie`, `access-control-*`, `link`, `refresh`, `service-worker-allowed`,
  source-map and manifest fields are never reflected. Bundles render only on site origins.
- **G11** Cross-origin reads from a site origin to a bare origin, to another site origin or
  to the API are blocked by G2/A6 (no CORS). Navigations and embeds between them are
  allowed; they transfer nothing.

### P. Public and member-only serving

- **P1** The control API is never on a listener that serves content. Public HTTPS serving
  uses its own listener and configuration; `/api/` does not exist there.
- **P2** Public serving is opt-in per root: only content explicitly published or marked
  public is served; a member's held cluster content is never implicitly public. A share
  link mentions a public URL only when that URL serves the root now.
- **P3** A members-only gateway is loopback/network-restricted or requires reader
  authentication (A8-style token or TLS client auth). "Not configured" means the daemon
  refuses the listener; libp2p membership gating never counts as HTTP protection.
- **P4** Public listeners serve held, verified bytes only; they never fetch unheld CIDs,
  live repos or name resolutions on demand (RASL, `/at/`, FISL included).
- **P5** Public active content needs per-root origins under the operator's domain (G3
  with `<cid>.<domain>`) or the G4 fallback; `localhost` tricks do not apply. RASL
  responses may add `Access-Control-Allow-Origin: *` on public listeners only (so browser
  clients can verify by hash), never on loopback, where it would let any page enumerate
  held content.
- **P6** Browsers trust the gateway/TLS; Nu-aware clients verify CIDs/proofs themselves.
  No document claims an ordinary browser performs Bao verification.

### X. Extraction (`nu get -o`)

- **X1** Tree paths must be relative, with no empty, `.`, `..`, NUL or backslash segment
  and names ≤ 255 bytes; a violation is `invalid_argument` and nothing is written.
- **X2** Every existing component under the destination is `Lstat`ed; a symlink on the
  way, or in place of a file being written, aborts the extraction.
- **X3** Symlink entries are never created; they are reported as skipped with their target.
- **X4** A name the filesystem maps onto an existing entry this run did not write (case
  folding, Unicode normalization) is reported as skipped naming the entry it collides
  with; a skipped directory's contents are skipped too. Nothing is merged or renamed
  silently. Pre-existing files of exactly the same name are replaced atomically.
- **X5** Modes are `0644`/`0755` (executable bit only); no ownership, xattrs, times or
  special files. Writes go to a temp file in the target directory and are renamed.
- **X6** The report (files, dirs, bytes, skipped with reasons) is returned in the
  operation result and printed by the CLI; `get` is not "success" if skipped is non-empty
  without saying so.

### Negative-test matrix

Automated rows name their Go test; "browser" rows are in the
[browser plan](validation.md#browser-origin-isolation-checks-0035); "gate" rows are
required by the named task before it ships.

| # | Rule | Attack | Expected | Coverage |
|---|---|---|---|---|
| N1 | A2 | API request with `Host: nu.example.com:<port>` (rebinding) | 403 | `api.TestLoopbackProtections` |
| N2 | A2 | API request with loopback host, wrong port | 403 | `api.TestLoopbackProtections` |
| N3 | A4 | POST with `Origin: http://127.0.0.1:8171` (gateway origin) | 403 | `api.TestLoopbackProtections` |
| N4 | A4 | POST with `Sec-Fetch-Site: cross-site` / `same-site` | 403 | `api.TestLoopbackProtections` |
| N5 | A4 | `OPTIONS` preflight | 403, no CORS headers | `api.TestLoopbackProtections` |
| N6 | A5 | POST `/pins`, `/dag` as `text/plain`, form or multipart, or no type | 400 | `api.TestLoopbackProtections` |
| N7 | A6 | any response | no `Access-Control-*`, `nosniff`, `no-store`, no `Set-Cookie` | `api.TestLoopbackProtections` |
| N8 | A3 | GET with foreign `Origin` | 200, unreadable by the page (no CORS) | `api.TestLoopbackProtections` |
| N9 | A1 | daemon with `api.listen = 0.0.0.0:…` or `gateway.listen = [::]:…` | refuses to start, action names loopback | `daemon.TestDaemon` |
| N10 | G1 | gateway `Host` not loopback/`<cid>.localhost`, or `evil.localhost`, or wrong port | 403 | `gateway.TestGatewayOrigins` |
| N11 | G3/G4 | `/tree/<cid>/index.html` on bare origin | octet-stream attachment + `Link` alternate | `gateway.TestGatewayOrigins` |
| N12 | G4 | SVG/XML/JS/PDF types on bare origin | attachment | `gateway.TestMimeRules` |
| N13 | G7 | hostile `mime` (`text/html; charset=x-evil`, `foo`, `a/b/c`, CRLF) | sanitized or octet-stream | `gateway.TestMimeRules` |
| N14 | G3 | `<cid>.localhost/` serves `index.html` as HTML with no listing CSP | 200 `text/html` | `gateway.TestGatewayOrigins` |
| N15 | G8 | `/sub/../index.html`, NUL, empty segments | 400 | `gateway.TestGatewayOrigins` |
| N16 | G6 | `//evil.example` on a site whose tree has `evil.example/` | 400, no `Location` | `gateway.TestGatewayOrigins` |
| N17 | G6 | directory without slash | 301 to same-origin path | `gateway.TestGatewayOrigins` |
| N18 | G7 | symlink entry | 422, listed | `gateway.TestGatewayOrigins` |
| N19 | G8 | entry name with NUL / `..` in a listing | shown, not linked | `gateway.TestGatewayOrigins` |
| N20 | G2 | POST/PUT/OPTIONS with foreign `Origin` | 400, no CORS, no cookie | `gateway.TestGatewayOrigins` |
| N21 | G9 | `If-None-Match: "<cid>"` | 304 | `gateway.TestGatewayOrigins` |
| N22 | G3/G11 | script on `<cidA>.localhost` reads `localhost:8171/tree/<cidB>/`, the API, or `<cidB>.localhost` | blocked | browser B2–B4 |
| N23 | A4/A5 | script on a site origin POSTs to the API (JSON and `text/plain`) | no state change | browser B5 |
| N24 | G3 | cookies/localStorage set on `<cidA>.localhost` | not visible on `<cidB>.localhost` or `localhost` | browser B6 |
| N25 | G4 | browser without `*.localhost` (Safari?) opens the bare origin | downloads, never renders HTML | browser B1/B7 |
| N26 | G5 | name route on a bare origin | non-active fallback | gate: first name route |
| N27 | G10 | MASL `content-type: text/html` on bare origin; `set-cookie`/`location`/CRLF in values; CSP that loosens | download; dropped; intersected | gate: 0055 |
| N28 | P1–P3 | public listener configured on a members-only node without reader auth | refuses to listen; `/api/` absent on public listener | gate: 0043 |
| N29 | P4/P5 | RASL for unheld CID on a public seed; CORS on loopback RASL | 404, no fetch; no CORS header | gate: 0052 |
| N30 | G5 | `/at/` record with HTML/script in a string field | inert JSON/text, CSP | gate: 0053 |
| N31 | X1 | `../x`, `/x`, `a/../../x`, `a\..\x`, NUL, `./x` | invalid_argument, nothing written | `extract.TestExtractSafety` |
| N32 | X2 | symlink in destination (dir on the way, file being replaced) | aborts, nothing through the link | `extract.TestExtractSafety` |
| N33 | X3 | symlink entry | skipped, reported | `extract.TestExtractSafety` |
| N34 | X4 | `a.txt` and `A.txt` in one directory | both written on case-sensitive FS; second skipped naming `a.txt` on case-insensitive FS | `extract.TestExtractSafety` (Linux); macOS/APFS: browser plan B8 |
| N35 | X5 | mode `0777`, setuid in tree | `0755` only | `extract.TestExtractSafety` |
| N36 | A8 | producer listener: push without a token, with another collection's token, with a revoked token | 403, nothing queued | `main.TestProducerPushCLI` |
| N37 | A8/A2 | producer listener: `Host` not in `api.producer.hosts` | 403 | `main.TestProducerPushCLI` |
| N38 | A8 | producer listener: any non-push route (`/status`) | 404 | `main.TestProducerPushCLI` |
| N39 | A8 | `api.producer.listen` without `trusted_network` or `hosts` | refuses to start, action names the key | gate: TLS (0043) |

### Feature-entry gates

- **0028 (v0)**: A1–A7, G1–G4, G6–G9, G11, X1–X6 implemented; matrix rows with a named
  test pass in CI; browser rows B1–B7 recorded in validation.md for Chromium and Firefox
  (Safari may be recorded as "no `*.localhost`; fallback verified"). Until recorded, the
  site origin is *available* but the only *claim* is the fallback.
- **0043 (public HTTPS)**: A8 (if any non-loopback API ships), P1–P6, N28; browser rows
  rerun against the public origin scheme in all three browsers; cert/deployment runbook.
- **0052 (RASL)**: N29; RASL on loopback has no CORS; the SSRF rows of 0061 for `rasl://`.
- **0053 (atproto)**: N30; `/at/` has no site origin and blobs are downloads (G5).
- **0055 (MASL bundles)**: G10 in full, N27, bundle rendering only on site origins,
  `.tile` headers through the same allowlist.
- **First name route** (names track): G5 label encoding and N26 before any name path
  renders anything but the fallback.

### Independent review plan

Before each gate above, a reviewer who did not write the code: (1) reads this section
and api.md and diffs them against `api/`, `gateway/`, `node/extract/` and
`node/daemon/` (listener refusal), noting any rule without an enforcing line of code;
(2) runs the matrix: `go test ./api/ ./gateway/ ./node/extract/ ./node/daemon/` and the
browser plan, attaching results to validation.md; (3) tries five attacks not in the
matrix (suggested: Host with trailing dot or uppercase, `Origin: null`, chunked bodies,
`Range` on listings, a tree whose names are 255-byte Unicode) and adds any that bite as
rows; (4) checks that every new route since the last review is in the route table, the
OpenAPI document and (if it changes state) rejected by N3–N6; (5) signs off in the task's
work log with the commit reviewed. A gate without a signed review is not met.

### Shared HTTPS source policy

`httpsrc` is the common streaming GET client for URL ingest and future RASL/FISL/atproto
consumers. HTTPS is mandatory unless explicitly configured otherwise. It uses no ambient
proxy, cookies, credentials, decompression or connection reuse. Each connection resolves
once, rejects any forbidden address in the answer, then dials a checked IP literal (TLS
still authenticates the URL hostname). Loopback, private, link-local, unspecified,
multicast, CGNAT, IPv4-mapped and special-use/transition addresses are denied; well-known
NAT64 addresses are checked against their embedded IPv4, and local-use NAT64 is denied.
Trusted configuration can exempt explicit IP prefixes. Per-call host allowlists are exact,
case-insensitive hostnames/IP literals (a trailing DNS dot is ignored), not suffix matches;
no list permits all hosts, an empty list permits none. Allowlisting does not exempt IP
checks. Every redirect is checked before DNS/dial, with five hops by default. URLs are
limited to 8 KiB and response headers to 64 KiB (Content-Range to 128 bytes).

Calls require positive maximum bytes, overall timeout and network idle timeout. The byte
budget includes any retained prefix; size overflow never writes beyond that budget. Sinks
must cooperate with cancellation rather than block indefinitely. Body bytes stream to a
caller-owned writer, with no whole-body buffer. A store import can feed `AddBlob(r, -1)`
through `io.Pipe`, propagating fetch/store errors with `CloseWithError`; no store hook is
required. Callers must not publish partial or failed transfers. Import policy chooses the
BLAKE3 byte budget (there is no protocol cap); known-CID whole-object fetches and SHA-256
imports retain the 8 MiB limit.

Resume is caller-driven, using the previous final URL and retained prefix length. A strong
ETag enables `Range`/`If-Range`; Last-Modified is usable only without any ETag and when the
stored response Date is at least 60 seconds later (RFC 9110). Otherwise restart at zero.
Redirects clear range validators; a `200` also resets the sink, never appends. A `206` must
cover exactly the requested suffix through a declared total, within the size budget, with
no contradictory validator. `416` and malformed ranges fail rather than imply completion.
The result records final URL, ETag, Last-Modified, Date, content type and streamed size,
including partial progress after body errors. Validators are not integrity proofs.

Callers report verification failures against the **final origin**. The in-memory scoreboard
marks it demoted for five minutes, retaining at most 256 origins (earliest expiry evicted).
Schedulers may prefer other sources; fetching is not automatically forbidden. In-process
tests assert that denied hosts never reach DNS/dial; private-mode packet-capture evidence
remains part of the private-mode exit, not this package's acceptance evidence. Item URL
imports (0059) are covered in process by `collection.TestItemPush` (unlisted hosts and
redirects to them are never looked up) and in a network namespace by
`collection.TestPrivateIngestEgress` (`-tags natlab`; nftables counters show packets only
to the `ingest.hosts` address and no DNS query); a packet capture on a multi-host private
deployment is CollGS pilot evidence (0063).

## Acceptance scenarios

| Scenario | Required outcome |
|---|---|
| First share on fresh Linux/macOS workstations | Account-free add/share/get/browser journey succeeds; failure explains a next step |
| Publish, wait, sleep | Fresh third client retrieves the exact seed-confirmed CID |
| Crash during sync/pin switch | Restart exposes complete old/new held view, never mixed content |
| Missing members/zones, stale evidence or partition | No false readiness; protected old/surplus copies are not intentionally released |
| Lost node with repair capacity | Degradation is reported; surviving content restores policy with online repair authority |
| No online repair authority | Explicit alert and recovery action, not apparent automatic repair |
| Disk full, false sizes, rapid retargets | Enforced budgets, preserved held content, bounded staging and actionable pause |
| Historical entry without complete content | Honest availability; no false immediate-restore success |
| Interrupted/missing-baseline bundle | Resume or missing-content report; held version remains usable |
| Key/state restore to clean machines | Verified retained versions can be recovered from survivors or complete export |
| Concurrent update and drain | Visible conflict/epoch changes and conservative release behavior |
| Upgrade interrupted or incompatible | Documented store/protocol migration and recovery; no implicit destructive downgrade |
| PDS outage/key change/reconnect | Cached/CID retrieval remains usable; freshness and verification limits remain visible |
| Malicious browser page or extracted tree | Cannot operate control API or escape the requested destination |

Evidence, environments and published support limits belong in [validation](validation.md).
