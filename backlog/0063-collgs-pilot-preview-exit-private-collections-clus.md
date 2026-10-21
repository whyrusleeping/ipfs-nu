---
id: "0063"
title: 'CollGS pilot preview exit: private collections cluster usable by vmx'
status: todo
priority: 1
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0057"
    - "0059"
    - "0034"
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
