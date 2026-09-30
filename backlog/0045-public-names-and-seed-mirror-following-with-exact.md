---
id: "0045"
title: Public names and seed/mirror following with exact-target readiness
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0028"
    - "0035"
    - "0036"
    - "0051"
spec_refs:
    - docs/names.md#Identity and names
    - docs/follow.md#Following datasets
    - docs/operations.md#Reader-visible versions
---

## Description
Implement atproto OAuth scoped login, node registration, tree/file/blob name records, publish/resolve with proofs/cache and seed follow. Full/scoped/lazy modes, observed/syncing/held state, held-view atomic switches, exact-target seed waits, resource budgets and actionable lag/deletion/unfollow lifecycle use shared store/transfer. Initial polling/proof paths first; peer watch/swarm/Jetstream scalability extends same model, not new sync stack. DID-wide form ships only after explicit future-name/non-tree/aggregate-quota rules.

Acceptance: publish + seed-completion wait + laptop sleep + fresh third retrieval of acknowledged CID; stale cached resolution without name-plane requests is labeled; crash switches and quota stops preserve held view; named get binds one CID while follow gateway exposes held root; orphan pins visible; mocked and real OAuth/PDS tests. Explicit held/offline read selection differs from default resolved-target get and always exposes selected CID. No public-name history, rollback-prevention or publication-as-readiness promise. Account-free seed availability is fixed-CID cluster/client pin with content wait; outsider sharing requires public mode and explicit announce/public HTTPS consent. Private pins remain members-only. Named publication requires atproto login.

## Acceptance criteria

## Work log
