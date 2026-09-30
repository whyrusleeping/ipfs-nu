---
id: "0032"
title: 'Spec D20: clusters (IPFS Cluster successor) incl. private/isolated networks'
status: in_review
priority: 3
created: "2026-09-29"
updated: "2026-09-29"
depends_on: []
spec_refs:
    - docs/cluster.md#D20. Clusters
---

## Description
Design a successor to IPFS Cluster: managed collections of data (datasets) replicated across member nodes and updated over time, with the ability to run fully private / isolated (no DHT, no public infra, no atproto).

Done in docs/cluster.md (D20), cross-refs in spec.md (map, scope, UX, architecture, sharing table, layout, milestone 2b, open decisions), data.md D15/D17, names.md D4, networking.md D8, follow.md nav.

Key choices for review:
- Cluster state = LWW map merged from per-key signed repos (atproto repo format, did:key signers, cluster-local); root key from genesis, admins, per-dataset writers; revocation by commit cutoff. No Raft, no op-log CRDT.
- Datasets: local (heads in cluster state) or external Nu names (D19 follow); fixed CID pins as static local datasets.
- Placement per blob by weighted rendezvous hashing over eligible members (tags, spread, capacity); structure everywhere; no per-blob allocation state.
- Replicated-version guarantee: old head retained until new head fully replicated.
- Handoff (surplus kept until new owners confirm), auto-repair by admin daemons marking members unavailable, drain, Bao-based audits.
- Network modes public/private/isolated; QUIC means no pnet PSK → mutual-auth gating; egress-safety CI.
- Air-gap bundles (diff CARs, data-diode friendly), pinned atproto signing keys for isolated name tracking.

Acceptance: user review; open items in cluster.md "Details still to specify" (notably opt-in version retention).

## Acceptance criteria

## Work log
