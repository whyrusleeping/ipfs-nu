---
id: "0058"
title: Follow collections with local selection rules (`nu follow --select`)
status: done
priority: 3
created: "2026-10-20"
updated: "2026-10-22"
depends_on:
    - "0057"
    - "0045"
spec_refs:
    - docs/collections.md#Following collections
    - docs/follow.md#Follow scopes and quotas
---

## Description
Extend public follows with `nu follow <name> --select rules` for collections (docs/collections.md#following-collections): pin proof chain + full catalog + rule-selected items, reusing 0057's rule engine and D19 delta sync; `--path`/`--max-size` compose; rescue rules rejected outside clusters. Acceptance: rolling/regional rules converge on a follower after restart and missed notifications; completeness marks for partially selected subtrees specified and tested.

## Acceptance criteria

## Work log
- 2026-10-22: Done (6dccb6c, d39ffa6, 35e954b; merged). nu follow <name> --select rules: a select follow is a collection dataset whose single writer is the followed name; facets from the publisher header; local rules, max_size budget, rescue refused; catalog held complete under nu-catalog, items under nu-tree. Evidence: collection.TestFollowSelection, node.TestFollowSelect, nutest.TestFollowSelectNetwork (only selected items fetched; partition, restart, missed publication converge). Note: ls now lists a held directory without fetching below it (global change). Product questions raised: ls semantics, rules CLI verb, --replicas with select follows.
