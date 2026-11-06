---
id: "0098"
title: 'Opt-in field NAT telemetry: profile pair → strategy → outcome, time to direct (split from 0025)'
status: todo
priority: 3
created: "2026-11-06"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
Agent-doable part of 0025: opt-in field telemetry (off by default) recording NAT-profile pair → strategy → outcome and time to direct. No content, no PeerIDs, no IPs. Document exactly what is collected; let the per-pair table tool ingest the telemetry records alongside lab outcomes. There is no project-operated collection endpoint (0039), so the sink must be operator-configured or a local export.

Acceptance: off by default; enabling it records only the documented fields; a user can see what would be sent; the table tool can include telemetry rows.

## Acceptance criteria

## Work log
