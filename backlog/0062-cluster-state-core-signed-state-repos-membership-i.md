---
id: "0062"
title: 'Cluster state core: signed state repos, membership/invites, dataset records, writer heads, private-mode gating'
status: todo
priority: 1
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0009"
    - "0026"
    - "0040"
    - "0066"
    - "0051"
spec_refs:
    - docs/cluster.md#Cluster state
    - docs/cluster.md#Membership and access
    - docs/cluster.md#Network modes
---

## Description
**First step (moved from 0035):** specify invite token encoding, secret proof bound to PeerId, expiry, join credential handling and revocation semantics in docs/cluster.md; no private authority material in invites. Also implement the daemon's implicit **local cluster** (docs/cluster.md#D20. Clusters) and the rule of at most one networked cluster per daemon, with per-daemon unique dataset names; `nu dataset …` is the local-cluster form of `nu cluster dataset …`. Multi-node tests use `nutest` (0066).

Split from 0046: the cluster foundation that collections need, without placement or readiness evidence (0036). Per-authority signed state repos (byte-exact atproto v3, P-256 did:key, CAR verifies with stock atproto tooling), LWW merge with HLC and tombstones, root/admin/writer authorization, key/member/invite/dataset/head/config records, pre-authorized and secret invites + join, member gating for private/isolated modes (accept/dial only member PeerIds), state replication over `/nu/tree/1` head exchange + watch, signed heartbeats (liveness, free/used, skew), member status/egress safety checks. Union-head support hooks (per-writer heads readable separately) for 0057.

Acceptance: 3+ routed private-mode members converge signed state and reject unauthorized/out-of-scope records; invites/join/revocation work offline (pre-authorized) and online (secret); non-members cannot open streams; no public/default egress in the Linux isolated lab (packet capture); state/conflict/crash tests; CLI/API metadata acknowledgment reported separately from any content status. (These are 0046's state/membership/gating criteria.)

## Acceptance criteria

## Work log
