---
id: "0040"
title: Thin direct/hinted peer sessions for first sharing and private archives
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-10-20"
depends_on:
    - "0010"
    - "0060"
spec_refs:
    - docs/networking.md#D9. Peer sessions
    - Milestones
---

## Description
Implement minimal authenticated direct QUIC sessions using explicit/link/cache/LAN addresses and protocol stream routing. Retry/resume at request level. Private-mode explicit members use the same base without public lookup/relay/NAT defaults. Public full path race/relay upgrades extend this via 0020, not a prerequisite for a first direct/hinted fetcher.

Acceptance: LAN and routed peers transfer through sessions with bounded resources; dead addresses/corrupt identity explain failures; connection loss permits verified request resume; no dependency on Amino or aggressive NAT; explicit mode disables unauthorized/default discovery. Full relay-supported zero-config preview still requires the separate public-session work.

## Acceptance criteria

## Work log
