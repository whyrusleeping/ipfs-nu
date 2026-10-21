---
id: "0010"
title: '0a: identify (+push), ping, observed-address tracking'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0008"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
- identify `/ipfs/id/1.0.0` + identify push `/ipfs/id/push/1.0.0`: listen addrs, protocols, observed address, signed peer record, agent version.
- Observed-address tracking: aggregate observations per observer IP/port (this feeds AutoNAT v2 and the NAT profiling task).
- ping `/ipfs/ping/1.0.0`, used for RTT measurement and liveness.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- One smoke test in `experiments/` against go-libp2p: both sides see correct protocols and observed addresses; push updates propagate. (rust/js: 0011.)
- Fuzz target with seeds for the identify protobuf decoder.

## Acceptance criteria

## Work log
