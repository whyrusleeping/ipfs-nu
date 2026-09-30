---
id: "0050"
title: 'Scale track: dataset swarms, archival retention costs, bundles and placement benchmarks'
status: todo
priority: 3
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0045"
    - "0046"
    - "0047"
    - "0034"
    - "0037"
spec_refs:
    - docs/follow.md#Design target
    - docs/cluster.md#Design target
    - docs/validation.md#Validation and measurements
---

## Description
Extend shared name/tree/history machinery with large watch swarms, proof dissemination/Jetstream backstop, canonical swarm keys, full retention schedules/span index, scalable completeness/placement status, complete/incremental air-gap bundles and measured higher operating limits. Basic first share/archival is not gated by this task. Hard-NAT optimizer remains separate 0018–0025 workstream.

Acceptance: reproduce 10M-entry scattered churn and 1k-follower PDS-load benchmark with workload/environment/percentiles; measure physical history/staging/index/placement rescan cost and restore behavior; validate full vs changed-file transfer assumptions against pilot workloads before promoting sub-file reuse; missing bundle baseline/authority key rotation/interrupted import tests; publish progressively measured scale limits rather than claim PB/100-member support from synthetic targets alone.

## Acceptance criteria

## Work log
