---
id: "0066"
title: 'nutest: in-process multi-node test harness'
status: todo
priority: 1
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0065"
    - "0040"
spec_refs:
    - D18. Implementation layout
---

## Description
`nutest/`: start N real Nu nodes in one test process (temporary `$NU_PATH`s, loopback QUIC, real store/API) so transfer, cluster and collection behavior is testable by `go test` without root or network.

- Builders for N nodes with explicit peering (hints/member records) and optional mDNS off.
- Fault injection: stop/restart a node with its store intact, kill without clean shutdown, partition and heal pairs via connection gating, delay/bandwidth limits, injected clock skew, corrupt-data peers.
- Helpers to wait for conditions with timeouts and dump node status/operation events on failure.
- Deterministic seeds for anything random.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- Example tests: 3-node add → get with one node restarted mid-transfer; a partition/heal scenario; a corrupt-provider scenario.
- Starting a 3-node network takes well under a second; the harness's own tests finish in seconds without root.
- Documented in CONTRIBUTING.md (already referenced there).

## Acceptance criteria

## Work log
