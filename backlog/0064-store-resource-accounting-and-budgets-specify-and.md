---
id: "0064"
title: 'Store resource accounting and budgets: specify and implement logical/physical/transfer limits'
status: in_progress
priority: 2
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0009"
spec_refs:
    - docs/operations.md#Capacity and transfer budgets
    - docs/data.md#D17. Store and replication
---

## Description
Split from 0009/0036 so the core store and every first slice do not wait on accounting design.

Specify in docs/operations.md and docs/data.md (D17), then implement in `store/`:
- Logical quota (referenced bytes per dataset/scope, `max_size`), unique physical bytes charged once daemon-wide (blobs, outboards, DB/structure, retained versions, staging/partials, surplus) with precise attribution rules, and interval transfer budgets plus bandwidth/concurrency caps.
- Daemon-wide free-space reserve; staging bounds and cleanup of abandoned partials; preflight estimates that treat publisher size claims as unverified and count actual bytes.
- Pressure behavior: pause new acquisition, reclaim unpinned cache/abandoned staging, never evict held/retained/surplus/protected pins; report extra space needed and the resume action through the shared operation/status model.
- `--nocopy` invalidation accounting (mutated sources are not counted as held).

**Acceptance**
- Rules written in the owning docs with worked examples (shared blobs across datasets, history, staging).
- Disk-full, false-size, repeated-retarget and crash tests preserve held and retained pins, bound staging, and produce an actionable pause.
- Budgets are exposed via API/`nu status` with machine-readable shortfall.

## Acceptance criteria

## Work log
