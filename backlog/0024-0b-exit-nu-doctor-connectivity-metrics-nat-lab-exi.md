---
id: "0024"
title: '0b exit: `nu doctor`, connectivity metrics, NAT-lab exit scenarios in CI'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0020"
    - "0014"
    - "0003"
spec_refs:
    - D16. "Just works" is an engineering requirement
---

## Description
Operability for connectivity, plus the Phase 0b exit gate.

- **`nu doctor`:** UDP reachability, NAT profile (basic until profiling lands), relay latencies and reservation state, port-mapping result, IPv6, per-session path history ("why is X relayed?").
- **Metrics** (Prometheus): direct vs relayed bytes, time to first byte, time to direct, path switches, reservation uptime, probe outcomes.
- **Phase 0b exit scenarios in the NAT lab:** 100 % connect across the matrix (UDP-blocked excepted, with a clear `nu doctor` explanation); Nu↔Nu direct wherever both sides are EIM or one is public/IPv6/mapped; a transfer survives a Wi-Fi → hotspot style switch **without reconnecting**.

**Acceptance**
- The scenarios above run in CI and gate merges to the networking packages.

## Acceptance criteria

## Work log
