---
id: "0054"
title: 'atproto accounts as archive datasets: atrepo source, since-diffs, versions and redistribution limits'
status: done
priority: 2
created: "2026-09-30"
updated: "2026-10-22"
depends_on:
    - "0053"
    - "0047"
spec_refs:
    - docs/dasl.md#atproto accounts as archive datasets
    - docs/cluster.md#Datasets and updates
    - docs/follow.md#Update notification
---

## Description
An atproto account as a versioned archive dataset source (D21, D20 sources), for one-node archives and clusters.

- Source `{atrepo: did, nsids?, blobs?}`, head `{target: commit cid, kind: "atrepo", rev}`, versions = verified commits in the version tree. Unchanged records and MST subtrees are shared between versions.
- Updates: Jetstream (`wantedDids`, all collections), an infrastructure firehose (`#commit` after inversion validation) or polling as hints. Fetch `getRepo?since=<held rev>`, verify, and apply to a verified new `data`. Resync with a full export on chain breaks, `#sync` events, PDS migration or key rotation.
- Redistribution limits: for accounts the owner does not control, retained history is private (local/cluster) by default. Public serving tracks current verified state only and stops when the account is deactivated, taken down or deleted. Publishing history needs an explicit per-dataset override that is clearly reported. Own-account archives have no default restriction.
- Budgets for records, nodes, blob bytes and request rates per account/PDS. Status reports lag, the last verified rev, blob completeness and oversized (> 8 MiB) blobs skipped.

**Acceptance**
- Archive a busy real account over days: every version verifies, historical `/at/<did>@<rev>` reads work, and storage growth matches the changed records/blobs.
- Chain break, PDS migration, key rotation, deactivation and deletion scenarios behave as specified, including public-serving shutoff.
- Cluster replication of an atrepo dataset uses `/nu/tree/1` atrepo diffs rather than re-fetching from the PDS.

## Acceptance criteria

## Work log
- 2026-10-22: Mostly done (38372f7, 08ee7c1, b6cf0fb; merged dec2398). atrepo source records, getRepoStatus + getRepo?since (ApplySince) with full fallback and key-rotation re-resolve, a version per verified commit (archive schedule, observed rev/commit, nsids scope), blobs per policy, account status handling (deactivated/takendown/deleted stop updates, history kept, reactivation resumes), redistribution rules (unrestricted/current-only/private) enforced at share/public add, record-level diff, ls/cat <ds>@<v>/<nsid>/<rkey>, /c/ → /at/ gateway; polling only (no firehose). Evidence: node.TestAtrepoArchiveDataset, node.TestAtrepoClusterDataset, NU_LIVE TestLiveAtrepoArchive (why.bsky.world). REMAINING: cluster members replicate atrepo datasets over /nu/tree/1 (needs the atrepo profile in proto/ + fetcher atrepo walk); members currently report structure incomplete honestly and never refetch from the PDS. Also not built: public /at/ serving of current state, per-account budgets, PLC 404 handling. Product questions raised: public serving of atrepo current state, restricting ad-hoc snapshot share, own detection vs --own, --mirror with atproto source.
- 2026-10-22: Remaining bullet done (751fefb; merged): /nu/tree/1 profile atrepo (commit → layered MST → records; repo-key paths/ranges; held-commit have diffs; cursors through empty intermediates, specified in D14 with vectors + fuzz seeds), fetch/replica pass profile + last held commit with placement order and layer-1 atrepo fallback, atrepo+blobs blob placement. Evidence: node.TestAtrepoClusterDataset reaches 2-copy readiness for two revisions with exactly two PDS exports (admin only); second revision transfers 7 frames / 834 bytes + 5 have markers, asserted equal to the reachable-block difference. Not attempted (outside acceptance tests): multi-day live account run, public /at/ serving, per-account budgets.
