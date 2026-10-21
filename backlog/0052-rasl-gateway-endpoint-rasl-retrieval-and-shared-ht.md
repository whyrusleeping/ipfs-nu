---
id: "0052"
title: RASL gateway endpoint and rasl:// retrieval (on 0061 httpsrc)
status: done
priority: 2
created: "2026-09-30"
updated: "2026-10-21"
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
- 2026-10-21: Done on track/app (6002c14..8f9535b; merged a048040). rasl/ package (rasl:// parse incl. legacy web+rasl input, hint racing via httpsrc, verify-before-store, demotion), fetch.Source integration (HTTPS sources race peers; works daemonless), gateway /.well-known/rasl/{cid} (octet-stream, nosniff, attachment, Range, held-only, no CORS on loopback). Evidence: rasl.TestFetchObject (@dasl/rasl-shaped seed, redirects, liar demoted, size cap), rasl.TestSSRF (zero dials to private/rebinding hints), node.TestGetRASL (tree from hints only), gateway.TestRASLRoute. No live public RASL host verified (spec example host 404s). Deferred: share emitting rasl:// and public CORS (0043), browser WebCrypto row, @dasl/rasl automated cross-run, BLAKE3 RASL resume. Product questions raised: legacy web+rasl input, :port hints, private-cluster RASL, persisted source cache.
