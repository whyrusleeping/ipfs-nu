---
id: "0048"
title: Cluster repair, conservative handoff/drain, audits and operator recovery
status: todo
priority: 2
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0046"
    - "0047"
    - "0044"
spec_refs:
    - docs/cluster.md#Sync, handoff and repair
    - docs/operations.md#Acceptance scenarios
---

## Description
Implement protected old-version and surplus pins, safe release evidence, authenticated heartbeats, repair-authority daemons, failure/flap handling, drains and scrub/audits. Expose current/archive readiness, stale/unknown/degraded status, blocked capacity/spread and missing repair authority through CLI/JSON/metrics with recovery actions. Validate key/state restore and root re-genesis runbooks against actual cluster behavior.

Acceptance: kill member with surviving data/spare eligible capacity and online authority → truthful degradation then k/spread restoration; no authority → alert, not silent repair claim; partition/concurrent update/drain/full-owner tests never intentionally release protected copies from stale evidence; forced remove clearly destructive; sampled audit not full-content ack; clean-machine recovery and machine-readable error/alert tests.

## Acceptance criteria

## Work log
