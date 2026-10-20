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

CLI/API wait operations bind to an exact CID/version and requested condition, with timeout,
nonzero failure exit and machine-readable result. Report confirming holders, evidence age,
placement epoch and unmet requirements. A concurrent head change must not silently retarget
the wait. A seed wait means that seed fully holds the requested content, not that it merely
knows the name. Waiting for cluster metadata acknowledgments is a separate operation.

Readiness is conditional on trusted members' reports, policy feasibility and current
evidence. It is not consensus, indefinite availability, or proof of independent physical
storage. An audit shows timely supply of sampled bytes. Require sufficient eligible copies
and mandatory failure-domain spread; do not weaken policy when a node fails or during setup.
If strict spread cannot be satisfied, report the shortfall. Best-effort copies, such as
member-selected collection holdings ([D22](collections.md#guaranteed-copies)), are reported
as holder counts and never use the same green status.

Before update/handoff may release protected content, specify fresh authenticated evidence
bound to target, retention live set, policy and placement epoch; stale, partitioned or
conflicting state must not authorize release. Missing/unknown evidence keeps protected
pins, subject to a visible disk-pressure pause. The evidence/coordination protocol is still
to specify and fault-test; LWW state alone does not establish a safe release decision.

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

Keep these controls distinct:

1. **Logical quota:** referenced file bytes for the selected dataset/scope. `max_size` /
   `--max-size` means this limit, not a cumulative network budget.
2. **Physical budget:** unique local blob bytes plus outboards, structure/DB, retained versions,
   staging/partial downloads and surplus. Charge shared physical bytes once daemon-wide;
   per-dataset logical totals may count repeated references. Attribution rules need precision.
3. **Transfer budget:** downloaded bytes over a stated interval and bandwidth/concurrency caps.
   Repeated retargeting can consume it even while all versions fit the logical quota.

Preflight reports additional unique storage where known, uncertainty, staging headroom and
policy/capacity shortfalls. Publisher size fields are estimates until independently checked;
count actual bytes and stop at enforced limits. Do not trust a signed size claim as correct.
Reserve daemon-wide free space. On pressure, cancel/pause new acquisition, reclaim unpinned
cache/abandoned staging safely, and preserve held/retained/surplus pins. Report extra space
needed and the action to resume. Do not automatically remove archives to make a new update fit.

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

API and gateway default to loopback. Non-loopback API requires authenticated configuration;
a token alone is not a browser request-protection contract. Before shipping, specify Host/
Origin validation, cross-site request protections, token handling and DNS-rebinding defense.
A fetched page must not gain daemon privileges. Isolate untrusted active content by immutable
content/name origin and safe routing; define cookie, redirect, MIME and content-sniffing rules.
Mutable-name origin behavior and IP-literal/local browser fallbacks need explicit tests in
Safari on macOS as well as Firefox/Chromium. Until origin isolation is proven, path hosting
serves downloads/non-active content or a tested sandbox/CSP fallback, not unrestricted sites.

Public HTTPS deployment is opt-in and must separate control API from content serving. Member-
only gateways require explicit reader authentication or remain local/network-restricted;
cluster libp2p gating does not protect an accidentally exposed HTTP listener. Verify fetched
bytes before serving; ordinary browsers trust the gateway/TLS, while Nu-aware clients verify
content/proofs independently. Do not claim an ordinary browser performs Bao verification.

DASL/atproto surfaces follow the same rules ([D21](dasl.md)). RASL responses are
`application/octet-stream`/`nosniff` and cover only content already served publicly. MASL
headers pass an allowlist and never loosen gateway policy. Bundles and `.tile`s render only
on isolated origins. atproto records render as inert JSON/text. Public seeds never fetch
unheld CIDs or live repos on demand. Third-party atproto archives serve current verified
state publicly only while the account is active; retained history stays private unless an
explicit override is set. RASL/FISL/`at://` fetches apply SSRF guards to URLs, redirects and
DID-document endpoints.

Extraction must reject path traversal and unsafe symlinks; cross-platform filename/metadata
handling must report transformations rather than silently produce a different archive.
OAuth/discovery SSRF and malicious endpoint validation are prerequisites for names.

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
