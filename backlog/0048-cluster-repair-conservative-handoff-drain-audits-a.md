---
id: "0048"
title: Cluster repair, conservative handoff/drain, audits and operator recovery
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
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
- 2026-10-22: Done (cefb5d1..f1b9d61; merged, coordinator resolved replica.check profile + held map and displaced heads). Repair (admin with cluster.repair=auto marks silent/failed members unavailable after down_after, restores after up_after), safe release of old versions and surplus blobs behind release.settle and k confirmed owner copies (release.auto default false; explicit dataset gc + --dry-run), drain/rm with refusals and --force reports, peer audits + local scrub (corrupt dropped and refetched), status alerts with actions, runbooks in docs/operations.md. Evidence: cluster/repair_net_test.go TestSafeRelease (R10, R11, R12, R14), TestReleaseRules (R11, R13), TestDrainRemove (R16), TestAuditScrub (R18), TestRepairAfterLoss (R20, lost node, no authority), cmd/nu.TestDatasetGCCLI. Not done: collection surplus release, crash between release decision and delete, natlab repair traffic, Prometheus metrics, injectable clock. Findings: full store blocks signed-state writes; paused owner blocks surplus release; audits prove serving not storage. Product questions raised: release.auto default, headroom for state writes, partitioned authority marking.
