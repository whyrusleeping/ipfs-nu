---
id: "0047"
title: Integrated archive/mirror presets, version retention, retrieval and rollback
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-10-21"
depends_on:
    - "0009"
    - "0064"
spec_refs:
    - docs/product.md#Progressive experience
    - docs/cluster.md#Version history
    - docs/operations.md#Retention and historical availability
---

## Description
**Also owns (moved from 0036):** specify, before implementing, immutable version identity and collision validation, annotation (label/note) authority, mirror/archive preset defaults (provisional until 0034 pilot data), skipped/never-completed version status, labeling of missing content with explicit reacquisition, and retained historical identity/signing-key provenance. One-node archives are `nu dataset create --archive` in the daemon's local cluster (docs/cluster.md#D20. Clusters), not a separate code path; one-node readiness is local completeness and does not wait on 0036.

Implement common version-tree and retention/availability machinery with easy mirror/archive presets and one-storage-member archival path; no new sidecar/datastore/transfer family. Provide immutable snapshot/label/time lookup, versions/diff, label content protection, dry-run pruning/removal and rollback as a new head. Cluster integration uses same history and owned-live-set accounting; basic keep-last/labels before full scale-track schedules/span-index optimization. External source history is observed-only.

Acceptance: individual uses explicit one-copy archive preset on Linux/macOS (APFS crash/sleep/Unicode/case tests), imports two versions and reads/restores exact old bytes without PDS or multi-node setup; incomplete/never-replicated/pruned entries do not claim immediate retrieval; labels and held/safeguard copies survive budget pressure; shared references prevent unsafe deletion; time selection reports unavailable chosen version rather than silently falling back. Immutable snapshot identity versus editable annotations/collisions, skipped versions and explicit missing-version reacquisition tested; provenance/key-evidence and authenticated import-manifest export plus crash consistency tested; placement integration waits for cluster implementation, not a second archival subsystem.

## Acceptance criteria

## Work log
