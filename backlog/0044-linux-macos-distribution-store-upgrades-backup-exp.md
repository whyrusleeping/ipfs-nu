---
id: "0044"
title: Linux/macOS distribution, store upgrades, backup/export and clean-machine recovery
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0009"
    - "0035"
    - "0036"
spec_refs:
    - docs/operations.md#Authority, administration and recovery
    - docs/operations.md#Acceptance scenarios
    - Milestones
---

## Description
Deliver workstation install/service lifecycle/update UX on Linux/macOS and Linux storage operations. Define/test filename/Unicode/case/symlink/mode handling and no-copy invalidation. Implement consistent content export/import plus separate authority/signed-state backup/restore, format/version compatibility policy and interrupted upgrade recovery. Document fixed-root loss/compromise re-genesis instead of pretending root rotation exists.

Acceptance: native architecture/APFS crash/sleep/Unicode tests plus signed/notarized macOS packaging and upgrade workflow; exact bytes/tree metadata or explicit transformation reports across platforms; clean-machine restore from surviving content/complete export and backed-up authority/state; upgrade/crash tests, documented unsupported downgrade behavior, source/key permissions and security review. Linux netns isolation results do not authorize macOS isolation claims. Cluster-aware recovery integrates with cluster implementation, not independent storage model.

## Acceptance criteria

## Work log
