---
id: "0054"
title: 'atproto accounts as archive datasets: atrepo source, since-diffs, versions and redistribution limits'
status: in_progress
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
