---
id: "0017"
title: '0b: disco `/nu/path/1`: candidates, authenticated probes, path selection, continuous re-probing'
status: in_progress
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0015"
    - "0010"
spec_refs:
    - docs/networking.md#D11. Nu-path
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description
The disco layer of nu-path (D11 steps 3–6, baseline part of D11c steps 2–3).

- `/nu/path/1` control stream on the nu-path connection: exchange candidates (LAN on all interfaces, global IPv6, observed, port-mapped) and NAT profiles (once the profiling task exists).
- Probe packets: non-QUIC packets on the same UDP socket (`Transport.WriteTo` / `ReadNonQUICPacket`), MAC'd with a key from the QUIC TLS exporter; ping/pong carries the observed address.
- ICE-style pair ordering (LAN → IPv6 → mapped → reflexive); aggressive early schedule (≈ 0/20/50/100/200/500 ms/1 s/2 s), then backoff; **never stop while relayed**: re-probe on address, profile or network changes and on a 30 s → 2 min → 10 min timer.
- Path selection + switching in magicsock; heartbeats on direct paths (interval from the measured mapping lifetime, default 25 s); instant fallback to the relay on loss.

**Acceptance (NAT lab)**
- Every pair where both sides are EIM, or one side is public/IPv6/mapped, goes direct, with time to direct reported.
- A 1 GB transfer survives an interface flap and a Wi-Fi → hotspot style address change with verified progress preserved. Report whether the inner QUIC connection stayed open (the D11 goal); the no-reconnect requirement is enforced by the 0024 networking exit, not here, so first share (0029) is not gated on it.
- Specify the probe security contract first (in docs/networking.md, moved here from 0035): replay resistance, destination authorization, rate limits; include the extensions 0021 needs for prediction/spraying. Forged/replayed probes and unauthorized destinations are rejected under it, not only by a MAC check. Safe limits and negative tests precede shipping; request independent review.

## Acceptance criteria

## Work log
- 2026-10-21: Baseline merged (ef28746, 94fc43e; 809f63e): p2p/disco with probe security contract in D11 (TLS-exporter MACs, nonces, replay window, destination authorization callback, rate limits), candidates, continuous re-probing, netmon RestartProbing hook; natlab EIM↔EIM and EIM↔APDF direct in 10–31 ms; link flap + address replacement recovered direct in ~1 s with one inner stream preserving progress (4 MiB). Remaining acceptance: public/IPv6/mapped pairs, 1 GB transfer through flap + Wi-Fi→hotspot-style change, independent review request.
