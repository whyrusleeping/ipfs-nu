---
id: "0098"
title: 'Opt-in field NAT telemetry: profile pair → strategy → outcome, time to direct (split from 0025)'
status: done
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

## Outcome

swarm.nat_telemetry = "off"|"on" (default off). On, each disco probe session appends one record to <repo>/nat-telemetry.ndjson (0600, 1 MiB cap, nothing sent): scenario "field", from/to IPv4 NAT-profile labels (local netcheck, peer via /nu/natprofile/1), strategy (lan/ipv6/mapped/probe/hard-nat), outcome direct (first direct path) or relayed (none within 60 s), time_to_direct_ns, nu version; no PeerIDs, addresses, timestamps or content. disco emits SessionStarted/SessionEnded and PathDirect Via/Hard. nu doctor shows the export path and records this run. nattable ingests field rows, reports them with strategy counts and never gates them. Documented in networking D11c §6. Verified by unit tests (exact fields, off by default, early-ended sessions unrecorded; nattable field rows) and a throwaway live two-swarm relay run that wrote "lan" direct records on both sides.

Commit: disco, swarm, nattable, docs: opt-in field NAT telemetry as a local address-free export (0098, split from 0025)
