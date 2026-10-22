---
id: "0063"
title: 'CollGS pilot preview exit: private collections cluster usable by vmx'
status: in_review
priority: 2
created: "2026-10-21"
updated: "2026-10-22"
depends_on:
    - "0068"
    - "0057"
    - "0059"
spec_refs:
    - Milestones
    - docs/validation.md#Pilot candidates
    - docs/collections.md#D22. Collections
---

## Description
Milestone 2a exit (spec.md#Milestones): a thin private-mode Nu build vmx's CollGS deployment can run. Scope is the union of 0060/0061/0062/0057/0059 plus their foundations; explicitly excludes rendezvous placement/readiness evidence (0046/0036), history, public networking, names and RASL gateway.

Acceptance: CollGS pilot criteria in docs/validation.md#pilot-candidates met on a multi-host private deployment running vmx's producer (pushes via item API) and his dashboard (or a port of it) against the holdings index API; index/push JSON API versioned and documented as the preview contract; published preview limits (trusted members, `replicas: 0` only, no guaranteed copies, measured throughput/catalog/index figures); install/init/invite/join runbook tested by someone other than the implementer; results recorded in validation.md.

## Acceptance criteria

## Work log
- 2026-10-21: Carried from 0057: produce the ≥20k items/day from ≥2 union writers run, measured catalog bytes/item, index cost and re-evaluation cost, and a multi-host private-mode run without multicast (0057 only has in-process evidence). Scaling notes from 0057: every writer head change walks that writer's whole catalog; item listing/status scan the whole items table; release sweep is a full store GC.
- 2026-10-22: Agent parts done (77c4b02..579d7a0 + merge fb5dbb7 with 0058; merged a680e40). Perf: incremental writer-catalog sync (head change 5.0 s → ~40 ms at 20k, 127 ms at 200k), pushes_todo queue index (push 109 → 260 items/s), items?holders=true; bug fixed: receiving members now hold every item in their own writer head. Figures (shared EPYC, load): 20k items pushed to two members at 119 items/s, none lost/duplicated, 0 conflicts, 2000 resends deduped, restart mid-run; catalog 934 B/item; collections DB 3.2 KiB/item (receiver); 200k queries: rarest page 294 ms, status 587 ms, facet query 4.1 s, full re-eval 11.2 s. Contract versioned collgs-preview-1 with preview limits; pilot runbook; scripts/pilot (collgs-cluster.sh SSH setup tested on loopback only; collgs-dash shim for vmx's dashboard API). Remaining (human): run on vmx's deployment with his producer/dashboard, multi-host run (needs hardware go-ahead), runbook tested by someone else. Product questions raised: shim vs subcommand vs port, transfer/rate fields, O(N) facet queries.
