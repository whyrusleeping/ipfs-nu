---
id: "0034"
title: 'Pilot workloads and product validation: first magic plus smaller archive/mirror teams'
status: in_review
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/product.md#Audience and jobs
    - docs/validation.md#Pilot workloads
    - Milestones
---

## Description
**Agent scope:** benchmark fixtures, workload-matrix templates, measurement methodology and result templates. **Human scope:** recruiting pilot teams and recording their real workloads; an agent must not invent pilot data or decisions. No implementation task depends on this one except the scale track (0050); presets and limits stay provisional until its data arrives.

Recruit concrete individual/publisher and institutional archive/mirror pilot workflows shaped like smaller Internet Archive/Wikipedia collections. Record file sizes, update/retention patterns, topology, hardware, operational constraints and IPFS dependencies. Create reproducible benchmark fixtures for sparse small objects, monolithic rewrites, appends and sharded indexes. Validate first-run add/share/get/browser ease as well as exact-version retrieval, restore and operator interventions.

Acceptance: documented workload matrix and pilot success criteria; repeatable fixtures; benchmark resource/transfer/storage/headroom/repair methodology and result templates; decision gate for sub-file reuse and capacity-adjustment UX. Publish no unmeasured scale support claims. External recruitment may need human coordination; fixture/method work can proceed.

## Acceptance criteria

## Work log
- 2026-10-22: Agent scope done (600ad9a, 0e34219; merged): docs/pilots.md (journeys J1–J5, workload shapes, per-gate success criteria with thresholds left for pilots, intake + result templates, method, decision gates for sub-file reuse and capacity UX with proposed — not adopted — triggers), bench/fixtures (8 seeded shapes), bench/cmd/fixturegen, opt-in runner bench/pilot (one-node archive, first-magic share, 3-member nutest cluster), nutest Node.Served byte counter; smoke run recorded as 'not pilot evidence' (whole-file amplification: rewrite 259×, shift 767×, sharded 420×; budget refusals correct; cluster transfer 1.5–1.6× novel vs 1.33× floor). Findings: per-file cost (71% CPU in store.Has from replica.check) → task 0078; SQLite file doesn't shrink after GC; metadata growth exceeds novel content for small updates. Remaining (human): recruit pilots, intake, thresholds, real runs, gate decisions.
