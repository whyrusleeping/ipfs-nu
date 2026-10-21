---
id: "0035"
title: Specify local API, gateway and browser security contract and safe extraction
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-21"
depends_on: []
spec_refs:
    - docs/operations.md#API, gateway and public exposure
    - docs/operations.md#Authority, administration and recovery
    - docs/networking.md#Details still to verify
---

## Description
Specify the local API, gateway and browser security contract (docs/operations.md#API, gateway and public exposure), plus safe extraction. Other security contracts moved to the tasks that implement them: OAuth/discovery validation → 0045, cluster invite/join/revocation → 0062, NAT probe destination authorization → 0017, root loss/re-genesis → 0044/0048.

- Threat model; API Host/Origin validation, cross-site request protections, token handling and DNS-rebinding defense for loopback and authenticated non-loopback APIs.
- Content-origin isolation: `<cid>.localhost:8171` subdomain origins (verify Safari/macOS, Firefox, Chromium resolution and behavior), mutable-name origins, IP-literal fallbacks; safe non-active path fallback until isolation is proven; cookie, redirect, MIME and sniffing rules; MASL header allowlist hooks.
- Public vs member-only HTTPS reader boundaries: control API separate from content serving; private gateways need reader auth or network restriction.
- Extraction: path traversal, unsafe symlinks, filename/Unicode/case transformations reported, not silent.

Acceptance: testable contracts and a negative-test matrix in the owning docs; browser test plan for Safari/macOS and Firefox/Chromium; ordinary fetched pages cannot operate the daemon API; public serving cannot silently bypass members-only policy; feature-entry gates for 0028/0043/0052/0053/0055 stated; independent review plan.

## Acceptance criteria

## Work log
- 2026-10-21: Done on track/app (f260510, merged c7ddf6a). Contract in docs/operations.md (threat model; rules A1–A8, G1–G11, P1–P6, X1–X6; negative-test matrix N1–N35 with 24 automated rows; feature-entry gates for 0028/0043/0052/0053/0055; independent review plan), mechanics in docs/api.md, browser plan B1–B8 in docs/validation.md with scripts/browser-check.sh. Fixes: JSON-only request bodies (closes form-POST CSRF hole), a real gateway open redirect (//host on site origins), escaped redirect/Link paths, unsafe names listed unlinked, extraction case/Unicode collision reporting. Not yet done (owned elsewhere/human): browser results B1–B8 (needs a person with Safari/Firefox/Chromium; gates active content on path routes and 0043), the independent review itself.
