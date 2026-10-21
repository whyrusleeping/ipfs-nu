---
id: "0052"
title: RASL gateway endpoint and rasl:// retrieval (on 0061 httpsrc)
status: todo
priority: 2
created: "2026-09-30"
updated: "2026-10-20"
depends_on:
    - "0028"
    - "0035"
    - "0061"
spec_refs:
    - docs/dasl.md#RASL
    - docs/operations.md#API, gateway and public exposure
---

## Description
DASL RASL support (D21), plus the `httpsrc/` package shared with FISL and atproto HTTP fetches.

- Gateway `GET`/`HEAD /.well-known/rasl/<cid>` for held DASL/BDASL CIDs (raw or DRISL): `application/octet-stream`, `nosniff`, no cookies or content negotiation, `Range`. Public seeds answer only for content they already serve publicly and never fetch unheld CIDs on demand.
- `nu get rasl://<cid>/?hint=h…`: parallel HTTPS hint fetches alongside Nu discovery, redirects treated as 307, verification by CID (whole object, 8 MiB limit, restart on failure).
- Uses `httpsrc` from 0061 (SSRF guards, bounds, per-origin demotion).
- `nu share` emits a `rasl://` link when an HTTPS seed serves the object.

**Acceptance**
- Interop with @dasl/rasl (serve and fetch both ways) and a plain web server using a 307 redirect.
- Browser test: WebCrypto verifies a SHA-256 object fetched via RASL from a Nu seed.
- SSRF tests (private IPs, DNS rebinding, redirect chains) pass; RASL responses cannot render active content.

## Acceptance criteria

## Work log
