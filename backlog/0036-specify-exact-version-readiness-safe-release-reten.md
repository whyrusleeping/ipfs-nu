---
id: "0036"
title: Specify exact-version readiness, safe release, retention availability and resource accounting
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on: []
spec_refs:
    - docs/operations.md#Readiness and status
    - docs/operations.md#Capacity and transfer budgets
    - docs/cluster.md#Details still to specify
    - docs/follow.md#Details still to specify
---

## Description
Translate accepted product contracts into wire/store rules: authenticated target/live-set/policy/epoch-bound completion evidence, freshness and policy feasibility, seed waits vs metadata waits, conservative protected/surplus release under partitions/concurrent epochs, full/scoped/lazy completeness, independent observed/held state, historical availability and conflict outcomes. Define logical/unique-physical/interval-transfer accounting, staging cleanup/free-space reserves, archive/mirror presets, labels/pruning and DID-wide follow lifecycle.

Acceptance: explicit feasibility/freshness/release rules with fault-test traces (missing zones/nodes, stale reports, partitions, concurrent heads/policy/drain, underreported sizes, disk full and repeated retargets); no LWW-only safe-release assumption; no same success for metadata ack and content readiness. Specify retained historical identity/key provenance. Resolve or explicitly gate impossibilities before storage/cluster implementation claims readiness. Specify first-held protection when no version has met policy, explicit one-node k=1 vs team k=2, strict-spread best-effort physical copies without false readiness, skipped-version status, labeling/reacquisition, immutable version identity/collision validation and annotation authority. Automatic protected release remains disabled until safe evidence tests pass.

## Acceptance criteria

## Work log
