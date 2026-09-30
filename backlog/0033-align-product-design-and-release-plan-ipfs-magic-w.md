---
id: "0033"
title: 'Align product design and release plan: IPFS magic with integrated archival and operations'
status: done
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on: []
spec_refs:
    - Goals and scope
    - Milestones
---

## Description
Apply accepted product review while preserving the user's clarified intent: delightful add/share/browser UX and institutional reliability are one product, not an archival pivot. Update split specs, add concise product/operational and migration contracts, prioritize thin sharing and trusted archival in parallel, add macOS and early HTTPS reach, reconcile implementation backlog and record missing accepted work. Preserve custom libp2p, Go daemon/CLI, atproto only for identity/names, and technical decisions not contradicted by the product review.

Acceptance: spec entry point remains an index; owning docs agree on readiness, quotas, historical availability and truthful durability; release gates include usability and recovery; IPFS migration is explicit and one-way; unsolved protocol/security mechanisms remain explicit blockers rather than fabricated solutions; backlog links/dependencies match new sequencing; independent consistency review and local markdown-link checks pass. No implementation or achieved-performance claims.

## Acceptance criteria

## Work log

- Reframed spec around first-class Share + Keep journeys with the user's clarified magic/fun + Real Users intent; preserved Go/custom-libp2p/atproto constraints.
- Added product, operational, migration and validation docs; reconciled data/names/follow/cluster/networking ownership and removed unconditional durability/history claims.
- Added Linux/macOS workstation and one-node archive coverage, early HTTPS browser/relay access, exact-target waits, safe resource/retention/recovery requirements, and explicit one-way IPFS provenance/cutover.
- Filed accepted implementation/design work 0034–0050; adjusted existing task descriptions/dependencies so hinted fetch/private archives do not depend on Amino/full NAT, security/readiness mechanisms precede claims, and production gate includes repair. Cleared dead Q/section references.
- Read-only independent product/consistency review completed; fixed its findings including first-held protection, immutable snapshot identity, strict spread feasibility, account-free public seed consent and opt-in telemetry. Unresolved mechanisms remain gated by 0035/0036, not claimed implemented.
- Local checks passed: 10 spec documents, 115 relative links/anchors, 50 task IDs/spec references, acyclic dependencies; `git diff --check` clean. No application build/tests run: repository remains design/backlog only. Existing D19/D20 review gates retained.
