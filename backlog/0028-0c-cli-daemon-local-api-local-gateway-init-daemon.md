---
id: "0028"
title: '0c: Networked first share: get/share between nodes, share links, daemonless get, local API security'
status: in_review
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0027"
    - "0065"
    - "0066"
spec_refs:
    - User experience
    - Local API, gateway and browsers
---

## Description
First-magic networked journey on top of the local skeleton (0065): the go-ipfs-style account-free add/share/get between machines. DHT and full public NAT sessions are optional integrations, not prerequisites for this direct/LAN/routed slice; public different-NAT reach is gated by 0029.

- Networked `get`/`cat`/`ls` via the fetcher (0027) with target parsing for `nu://` links; `share` and `add` print short links per docs/names.md#Links (CID + base36 PeerId, `addr=` only when no lookup source can supply addresses).
- Daemonless mode: when no daemon runs, `get`/`cat`/`ls`/`share` start an in-process node for the command; foreground `nu share` serves until interrupted and says content is available only while a holder is online.
- `nu status` gains connectivity and share sections; `nu doctor` basic diagnostics (peer unreachable, no addresses, UDP blocked) before the full NAT doctor (0024).
- Baseline API protections from 0065 plus gateway origin isolation via `<cid>.localhost` subdomains (non-active fallback where a browser does not resolve them). Safe extraction for `get -o`. The full 0035 contract lands before any non-loopback or public gateway (0043); v0 serves loopback only.

**Acceptance (v0 exit; deferred evidence is in 0068)**
- `nutest`: add → share → get of a file and a directory, including a recipient with no daemon and no prior `init`.
- A manual demo on two real machines on the same LAN or routed network (no NAT between them), recorded in docs/validation.md.
- The gateway serves a static site from a tree CID on a `<cid>.localhost` origin.
- A page served by the gateway cannot change daemon state through the API (baseline Host/Origin protections); `get -o` rejects path traversal and unsafe symlinks.
- Fresh-user studies happen at the public preview (0029), not here.

## Acceptance criteria

## Work log
- 2026-10-21: Implemented on track/app (7d3f33a, ed950cd, f6cdece; merged d121658). node/swarm assembles host+identify+lookup/mDNS+sessions+/nu/bytes server+fetcher into daemon and daemonless get/cat/ls/share; doctor connectivity checks. Evidence: nutest TestAddShareGet (file+dir, fresh recipient without daemon/init, gateway <cid>.localhost site, gateway-origin API DELETE → 403), cmd/nu TestAddSiteAndBrowse (foreground share, daemonless get, doctor), node/extract TestExtractSafety. Coordinator smoke with the built binary: two NU_PATHs over a LAN interface address, 20 MB tree fetched by a fresh no-daemon recipient in 0.38 s, identical; site origin renders; API rejects gateway origin. Remaining: the manual two-physical-machine demo (checklist in docs/validation.md) — needs the user.
