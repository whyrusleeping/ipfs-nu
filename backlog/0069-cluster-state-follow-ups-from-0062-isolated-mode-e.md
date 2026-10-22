---
id: "0069"
title: 'Cluster state follow-ups from 0062: isolated-mode endpoint refusal, cluster mDNS, join idle timeout, revoke/member CLI'
status: in_progress
priority: 3
created: "2026-10-21"
updated: "2026-10-22"
depends_on:
    - "0062"
spec_refs:
    - docs/cluster.md#Membership and access
    - docs/cluster.md#Network modes
---

## Description
Gaps left by 0062 (its acceptance is met; these are remaining parts of its described scope):

- Isolated mode refuses, at startup, any config naming a non-member endpoint (today it only turns mDNS off).
- Cluster-ID-derived mDNS discovery for private-mode members (p2p/lookup already supports cluster service names).
- Idle timeout for connections from non-members that hold a live-invite slot but never send `join`.
- CLI/API: key grant/revoke, invite revocation, `member ls|set|drain` (library support for key revocation exists).
- Public-mode content gating for non-announced cluster content beyond `/nu/cluster/1`.
- `replicas: all` encoding; check that a state-repo CAR verifies with stock atproto tooling (goat/indigo).

Acceptance: one fast test per item at the public boundary (nutest or natlab where networking is involved).

## Acceptance criteria

## Work log
- 2026-10-22: Partly done in 0048: member ls|set|drain|rm, key new|grant|revoke, invite --revoke CLI/API. Still open: isolated-mode endpoint refusal, cluster mDNS, join idle timeout, public-mode content gating, replicas: all encoding, goat/indigo state-repo CAR check.
- 2026-10-22: Done (6f11d3b..27ea682; merged fdb44fc). store.state_reserve (16 MiB, capped at limit/8) for signed state writes (cluster/reserve_test.go); isolated-mode startup refusal naming each offending key (node/daemon/isolated_test.go); cluster-ID mDNS in private/isolated mode (node/swarm/cluster_mdns_test.go); join-only idle timeout 10 s (cluster/join_idle_net_test.go); replicas: all vectors; stock indigo verifies an exported state repo (experiments/clusterstate_indigo_test.go). Public-mode gating decided minimally: all networked cluster datasets are member-only for state and public add; per-object gating of /nu/bytes|have|tree for non-members is deferred to the 'announce' feature (documented; in public mode a CID works as a read capability). Follow-ups: DHT/FISL config keys must join checkIsolated; DHT must not provide cluster dataset roots.
