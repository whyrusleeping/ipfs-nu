---
id: "0044"
title: Linux/macOS distribution, store upgrades, backup/export and clean-machine recovery
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
depends_on:
    - "0009"
    - "0035"
    - "0064"
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
- 2026-10-22: Agent-doable scope done (2a88741..c74b131; merged 116056c). Transactional migrations + interrupted-upgrade resume, newer store/layout refused (exit 4, no downgrade), nu repo export|import|backup|restore (content bundle with manifest vs 0600 authority backup), cluster.VerifyHeads, filename/Unicode/case reporting, release.sh (reproducible static tarballs + SHA256SUMS), systemd/launchd units, install.sh with --backup, release.yml with signing/notarization gated on named secrets. Docs: operations.md B1–B8, U1–U6, I1–I5. Evidence: node.TestCleanMachineRestore, cluster_test.TestRestoreFromSurvivors, store.TestInterruptedUpgrade, repo.TestNewerLayoutRefused, node.TestFilenameRoundTrip, extract.TestExtractSafety; manual install/upgrade with systemd user unit (found + fixed installer readiness bug). macOS-native, signing/notarization, root server install and security review moved to task 0073 (human/CI). Product questions raised: online export/backup, backup encryption, pin restore mode, pre-upgrade safety, macOS data dir.
