---
id: "0025"
title: '0b+ exit: real-hardware NAT lab, opt-in telemetry, published per-pair success table'
status: blocked
priority: 3
created: "2026-09-24"
updated: "2026-11-06"
depends_on:
    - "0021"
    - "0019"
    - "0003"
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
Measure hard-NAT work in the real world (D11c step 6), and produce the per-pair success table. This is the Phase 0b+ exit gate.

- **Real-hardware lab:** a handful of consumer routers, OpenWrt, pfSense/OPNsense with and without endpoint-independent NAT, and phone hotspots on two carriers (CGNAT). Scripted runs of the standard scenarios.
- **Opt-in field telemetry:** NAT-profile pair → strategy → outcome, time to direct. No content, no PeerIds, no IPs. Document what is collected.
- **Per-pair success table:** generated from NAT lab + hardware lab (+ telemetry when available), published every release.

**Acceptance (Phase 0b+ exit)**
- Direct for every lab pair where at least one side is EIM, public, mapped or on IPv6.
- ≥ 98 % for EIM ↔ random-EDM.
- The table is published; regressions fail the release checklist.

## Acceptance criteria

## Work log
- 2026-11-06: split the agent-doable parts. 0097 (done, 450dadf): software NAT-lab per-pair table, release gate, EIM↔random ≥98 % (100/100 locally after 2048-port search). 0098 (todo): opt-in field telemetry. Blocked on the rest: the real-hardware lab needs physical consumer routers, OpenWrt, pfSense/OPNsense (with/without EIM) and phone hotspots on two carriers. Unblock: a human with that equipment runs the standard scenarios and records `natlab.Outcome` pair records, which `p2p/natlab/cmd/nattable` already ingests; close 0025 once 0098 is also done.
