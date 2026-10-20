---
id: "0058"
title: Follow collections with local selection rules (`nu follow --select`)
status: todo
priority: 3
created: "2026-10-20"
updated: "2026-10-20"
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
