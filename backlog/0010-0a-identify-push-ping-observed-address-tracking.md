---
id: "0010"
title: '0a: identify (+push), ping, observed-address tracking'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0008"
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
---

## Description
- identify `/ipfs/id/1.0.0` + identify push `/ipfs/id/push/1.0.0`: listen addrs, protocols, observed address, signed peer record, agent version.
- Observed-address tracking: aggregate observations per observer IP/port (this feeds AutoNAT v2 and the NAT profiling task).
- ping `/ipfs/ping/1.0.0`, used for RTT measurement and liveness.

**Acceptance**
- Interop with go-, rust- and js-libp2p: each side sees correct protocols and observed addresses; push updates propagate.
- Fuzz the identify protobuf decoder.

## Acceptance criteria

## Work log
