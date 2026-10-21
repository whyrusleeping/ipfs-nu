---
id: "0061"
title: 'httpsrc: shared HTTPS fetching with SSRF guards, allowlists and resume'
status: done
priority: 2
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0001"
spec_refs:
    - docs/dasl.md#RASL
    - docs/data.md#D17. Store and replication
    - docs/operations.md#API, gateway and public exposure
---

## Description
Split from 0052 so URL import and collection pushes do not wait for the RASL gateway. `httpsrc/` package shared by RASL, FISL, atproto HTTP fetches, URL import and collection item pushes: refuse loopback/private/link-local targets (re-checked after redirects and DNS resolution) unless configured; per-call host allowlists (private-mode `ingest.hosts`); size/time bounds; Range/If-Range resume with strong validators; per-origin demotion after verification failures; streaming into the store's hashing writer.

Acceptance: SSRF tests (private IPs, DNS rebinding, redirect chains to unlisted hosts) pass; resume with and without strong validators; allowlist enforcement observable in a private-mode packet capture. (Carries 0052's httpsrc SSRF acceptance.)

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/http (b419679, merged). httpsrc: dial-time IP checks (SSRF, rebinding), redirect re-checks/cap, per-call host allowlists, size/time bounds, Range+If-Range resume on strong validators, per-origin demotion, streaming Sink. Evidence: TestSSRF, TestRebindingRedirectsAndAllowlist, TestResume, TestBoundsAndDemotion, TestStreamingStore. Allowlist enforcement shown in-process by asserting no dial attempts; the packet-capture evidence is deferred to the private-mode exit (0063). Behaviour documented in docs/operations.md.
