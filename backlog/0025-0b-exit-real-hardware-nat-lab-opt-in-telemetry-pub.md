---
id: "0025"
title: '0b+ exit: real-hardware NAT lab, opt-in telemetry, published per-pair success table'
status: todo
priority: 3
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0021"
    - "0019"
    - "0003"
spec_refs:
    - 'D11c. Hole punching: maximum effort'
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
