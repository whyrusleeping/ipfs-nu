---
id: "0035"
title: Specify security contracts for API/gateway, OAuth, cluster authority and NAT probes
status: todo
priority: 1
created: "2026-09-29"
updated: "2026-09-29"
depends_on: []
spec_refs:
    - docs/operations.md#API, gateway and public exposure
    - docs/operations.md#Authority, administration and recovery
    - docs/networking.md#Details still to verify
---

## Description
Define threat model and concrete API Host/Origin/CSRF/DNS-rebinding defenses, token handling, content-origin isolation and safe active-content fallback for path routes. Specify public/private HTTPS reader boundaries, MIME/redirect behavior, OAuth/discovery SSRF controls, safe extraction/metadata, cluster invite/revocation rules and authority recovery. Define replay-resistant destination authorization for aggressive NAT probes before 0021.

Acceptance: owning docs contain testable contracts and negative-test matrix; Safari/macOS and Firefox/Chromium origin tests, with non-active path fallback until isolation is proven; ordinary fetched browser pages cannot operate daemon API; public serving cannot silently bypass members-only policy; no private authority material in invitations; sampled audits not treated as full storage proofs; independent review plan and feature-entry gates. Do not invent global consensus/CAS or confidential membership promises.

## Acceptance criteria

## Work log
