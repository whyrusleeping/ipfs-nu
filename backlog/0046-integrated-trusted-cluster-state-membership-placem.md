---
id: "0046"
title: Integrated trusted cluster state, membership, placement and readiness
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0009"
    - "0026"
    - "0040"
    - "0035"
    - "0036"
    - "0051"
spec_refs:
    - docs/cluster.md#Cluster state
    - docs/cluster.md#Placement
    - docs/cluster.md#Status
---

## Description
Implement per-authority signed cluster-local repos (byte-exact atproto v3 repos signed by P-256 `did:key`s, built on the atproto MST profile; CAR exports verify with stock atproto tooling)/LWW merge, authorization/invites/membership and public/private/isolated gating with own libp2p. Implement deterministic blob placement, shared structure sync and fresh target/policy/epoch-bound completeness/status/waits per resolved contracts. Trusted archives must not depend on PDS, Amino or NAT optimization; initial private routed Linux deployment and one-node path first. Exact concurrent-write outcomes remain visible, not linearizable CAS.

Acceptance: 3+ routed members converge signed state, reject unauthorized changes, enforce k copies/required spread and report impossible/unknown policies honestly; one-node status never pretends default two copies exist; no public/default egress in Linux isolated lab; CLI/API metadata acknowledgment differs from content readiness; state/conflict/crash tests and placement vectors. Protected-version/surplus release follows 0036's tested evidence protocol, not bare LWW.

## Acceptance criteria

## Work log
