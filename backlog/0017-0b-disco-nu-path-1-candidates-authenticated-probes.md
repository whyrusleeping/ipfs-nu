---
id: "0017"
title: '0b: disco `/nu/path/1`: candidates, authenticated probes, path selection, continuous re-probing'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0015"
    - "0010"
spec_refs:
    - 'D11. Nu ↔ Nu: one QUIC connection over relay and direct paths ("nu-path")'
    - 'D11c. Hole punching: maximum effort'
---

## Description
The disco layer of nu-path (D11 steps 3–6, baseline part of D11c steps 2–3).

- `/nu/path/1` control stream on the nu-path connection: exchange candidates (LAN on all interfaces, global IPv6, observed, port-mapped) and NAT profiles (once the profiling task exists).
- Probe packets: non-QUIC packets on the same UDP socket (`Transport.WriteTo` / `ReadNonQUICPacket`), MAC'd with a key from the QUIC TLS exporter; ping/pong carries the observed address.
- ICE-style pair ordering (LAN → IPv6 → mapped → reflexive); aggressive early schedule (≈ 0/20/50/100/200/500 ms/1 s/2 s), then backoff; **never stop while relayed**: re-probe on address, profile or network changes and on a 30 s → 2 min → 10 min timer.
- Path selection + switching in magicsock; heartbeats on direct paths (interval from the measured mapping lifetime, default 25 s); instant fallback to the relay on loss.

**Acceptance (NAT lab)**
- Every pair where both sides are EIM, or one side is public/IPv6/mapped, goes direct, with time to direct reported.
- **A 1 GB transfer survives an interface flap and a Wi-Fi → hotspot style address change without the QUIC connection closing.**
- Forged probes (wrong MAC) are ignored.

## Acceptance criteria

## Work log
