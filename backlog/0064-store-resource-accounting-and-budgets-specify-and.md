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
- 2026-10-21: Done on track/app (4470879, e248ad8, 3b9c053; merged). Spec: docs/data.md D17 Accounting and budgets A1–A8; docs/operations.md C1–C5 + transfer budgets T1–T4. store: Limits, BudgetError admission before writes, MaxBlobSize, per-class Stats, Attribution, GCIfNeeded, Meter hook; migration 3 usage counters. Surfaces: insufficient_storage (507/exit 11), [store] config, status budget line, maintenance loop. Evidence: TestBudgetAdmission, TestKillMidWrite (accounting after crash), node TestStoreBudget, TestAccountingCounters, TestAttribution, TestMeter. Deferred to consumers: transfer-budget enforcement, per-scope paused status, attribution API route. Open product questions raised: free_reserve default (1 GiB), MaxBlobSize 8 TiB, synchronous pressure GC on admission.
