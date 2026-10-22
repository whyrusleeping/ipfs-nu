---
id: "0069"
title: 'Cluster state follow-ups from 0062: isolated-mode endpoint refusal, cluster mDNS, join idle timeout, revoke/member CLI'
status: todo
priority: 3
created: "2026-10-21"
updated: "2026-10-21"
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
