---
id: "0031"
title: 'Spec D19: following large, changing datasets (pin + track updates)'
status: done
priority: 3
created: "2026-09-29"
updated: "2026-10-21"
depends_on: []
spec_refs:
    - docs/follow.md#D19. Following large, changing trees
---

## Description
Specify pinning a named tree (e.g. a Wikipedia mirror) and following its updates.

Done in docs/follow.md (D19) with cross-refs in spec.md, names.md D7, data.md D14/D15/D17:
- Version ordering by commit rev; relayed proofs must be strictly newer; PDS answers authoritative.
- Publishing: incremental import cache, `nu tree edit`, publish coalescing (PDS write budget), retained structure for server-side diffs.
- Notification: `/nu/name/1` peer push of self-certifying proofs (primary), Jetstream (independent, unsigned hint), poll backstop.
- Dataset swarm key instead of per-blob providing.
- Delta sync: server-side `have` diff, client-driven batched fallback, completeness marks, atomic switch, retargeting.
- Scopes (--path, --lazy) and quotas.

Acceptance: user review of the design choices; open items listed under follow.md "Details still to specify".

## Acceptance criteria

## Work log
