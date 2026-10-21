---
id: "0057"
title: 'Collections (D22): entry metadata/facets, member-selected and rolling holdings, rescue, union heads, holdings index'
status: in_progress
priority: 2
created: "2026-10-20"
updated: "2026-10-21"
depends_on:
    - "0007"
    - "0065"
    - "0027"
    - "0062"
spec_refs:
    - docs/collections.md#D22. Collections
    - docs/validation.md#Pilot candidates
    - docs/data.md#D3. Directory trees
---

## Description
Preview scope (milestone 2a, 0063) is `replicas: 0`; combining collections with guaranteed floor placement lands with 0046.

Implement cluster collections per docs/collections.md for the CollGS pilot (docs/validation.md#pilot-candidates).

Scope: `meta` entry links and `collection` header facets in tree formats/walks; member-local selection rules (eq/in, min/max, within, bbox intersects; first match; hold=false; per-rule max_bytes) evaluated by reconciliation over the catalog, with collection-select/-manual/-conflict pin sources; rolling release; rescue rules; external decider API (item stream with cursor, hold/release); `replicas: 0` and additive self-selected copies that never count as `replicated`; `merge: union` heads with deterministic incremental merged root, writer-order conflict rule, `expire`, `drop`, coalesced head writes; per-member holdings log, `/nu/cluster/1 holdings` cursor/snapshot/resync, holdings index, CLI/API/Prometheus queries.

Acceptance: CollGS pilot criteria in validation.md (regional/cloud/rolling/rescue rules reproduced; ≥20k items/day from ≥2 union writers with no lost/duplicated items; convergence after restart/log overflow/late join; measured catalog bytes per item, index cost, re-evaluation cost; multi-host private mode without multicast). Open items in collections.md "Details still to specify" resolved or explicitly deferred with vectors for facets, predicates and merged roots.

## Acceptance criteria

## Work log
- 2026-10-21: Implemented on track/data (09eb9c9..e9189fc; merged 7264b33). collection.Manager (facets, selection rules, rolling/rescue/budget, union merge with conflicts/drops, holdings log + index), nu-catalog walk profile, cluster holdings op + heartbeat log positions, API/CLI (nu cluster collection …), metrics; decisions in docs/collections.md. Evidence: TestSelection, TestCollectionCluster (4-node private cluster: selection from two writers, rescue to 2 copies, rarest order, late join + restart convergence), TestUnionIncremental (300 random changes = reference merge), TestUnionVolume (600 items, two writers, exactly once), TestCollectionCLI; vectors in testdata/vectors/nu-collection. NOT yet evidenced (moved to 0063 pilot exit / 0068): ≥20k items/day rate, measured catalog bytes/item (rough: ~246 B), index and re-evaluation cost at scale, multi-host private mode on real machines. Remaining scope in docs/collections.md still-to-specify: partial items, remote index, ordered fetch sources, union-head history. Flake seen once: cluster TestPrivateCluster TempDir RemoveAll (not closed before cleanup).
