---
id: "0043"
title: Secure public HTTPS seed/gateway and browser sharing journey
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-21"
depends_on:
    - "0028"
    - "0035"
spec_refs:
    - Local API, gateway and browsers
    - docs/operations.md#API, gateway and public exposure
---

## Description
Expose explicitly public content over HTTPS with isolated active-content origins or safe fallback, tree listings/downloads and conventional Range. Return selected immutable roots/links; share emits browser links only when public serving is available, without silent upload or members-only exposure. Separate control API origins/listeners, authenticated private-reader surface and Nu client hash/proof verification from browser convenience trust.

Acceptance: ordinary browser without Nu opens published site/downloads a dataset from a configured seed; cross-site/API attack tests pass; private cluster gateway cannot bypass membership by accidental listener exposure; selected CID/held snapshot metadata and cache/Range behavior tested; deployment/cert runbook. Browser-native p2p/Helia adapter deferred; verified HTTP proof mapping must be designed before independent Bao claims.

## Acceptance criteria

## Work log
- 2026-10-21: Implemented on track/app (7cc7115, 244303f, de10bb0; merged 29995f3). [public] listener (TLS from operator files with reload, or behind_proxy on loopback), per-root public pins (nu public add|rm|ls), host-based origin isolation on <cid>.<domain>, RASL CORS only there, P2/P3/P4 refusals, limiter, share emits https/site/rasl links, runbook in docs/operations.md. Evidence: daemon.TestPublicListener (test CA, real daemon, listing, Range, site origin HTML, bare-origin download, RASL CORS, private-cluster refusal, unmarked CID identical 404), gateway.TestPublicListener (Host matrix N40, no API, no CORS outside RASL), TestPublicLimit, TestParseDomain, main.TestPublicCLI; matrix N28/N29 automated, N40/N41 new. Remaining: the ordinary-browser acceptance against a real public domain (B1–B9 incl. WebCrypto) — human step with a domain + cert — and independent review sign-off.
