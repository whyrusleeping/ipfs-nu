---
id: "0036"
title: Specify readiness evidence, exact-target waits and safe release
status: done
priority: 2
created: "2026-09-29"
updated: "2026-10-21"
depends_on: []
spec_refs:
    - docs/operations.md#Readiness and status
    - docs/operations.md#Capacity and transfer budgets
    - docs/cluster.md#Details still to specify
    - docs/follow.md#Details still to specify
---

## Description
Specify readiness evidence and safe release (docs/operations.md#Readiness and status, docs/cluster.md#Status). Resource accounting moved to 0064; version history identity, presets, labels and retention availability moved to 0047.

- Authenticated completion evidence bound to exact target, retention live set, policy and placement epoch; freshness windows; policy feasibility; unknown/degraded thresholds.
- Seed exact-target waits vs cluster metadata acknowledgments; full/scoped/lazy completeness; independent observed/syncing/held state.
- Conservative protected-version and surplus release under partitions, concurrent epochs, drains and concurrent heads; no LWW-only safe release. First-held protection when no version ever met policy; explicit one-node k=1 vs team k=2; strict spread with best-effort extra copies never reported ready.
- Conflict outcomes for concurrent head/policy writes (displaced intent visible).
- DID-wide follow lifecycle rules (future names, non-tree targets, aggregate quotas, recreation after deletion).

Acceptance: explicit feasibility/freshness/release rules in the owning docs with fault-test traces (missing zones/nodes, stale reports, partitions, concurrent heads/policy/drain); no same success for metadata ack and content readiness; impossibilities resolved or explicitly gated. Automatic protected release stays disabled until 0048's evidence tests pass.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/app (18a950c; merged). Contracts: docs/operations.md Readiness E1–E10, waits W1–W6, safe release S1–S8, matrix R1–R20 with gates for 0045/0046/0047/0048; docs/cluster.md completion-report heartbeat encoding; docs/follow.md seed waits, DID-wide follows; docs/api.md ContentState hook. Conservative defaults chosen (ready = every owner's fresh report; evidence.fresh 5 min; release.settle 10 min; release.auto=false) — open questions raised to the user: per-owner vs per-blob k counting, default timings, non-member public seeds satisfying seed waits.
