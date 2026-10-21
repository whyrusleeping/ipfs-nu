---
id: "0028"
title: '0c: Networked first share: get/share between nodes, share links, daemonless get, local API security'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0027"
    - "0065"
    - "0066"
    - "0035"
spec_refs:
    - User experience
    - Local API, gateway and browsers
---

## Description
First-magic networked journey on top of the local skeleton (0065): the go-ipfs-style account-free add/share/get between machines. DHT and full public NAT sessions are optional integrations, not prerequisites for this direct/LAN/routed slice; public different-NAT reach is gated by 0029.

- Networked `get`/`cat`/`ls` via the fetcher (0027) with target parsing for `nu://` links; `share` and `add` print short links per docs/names.md#Links (CID + base36 PeerId, `addr=` only when no lookup source can supply addresses).
- Daemonless mode: when no daemon runs, `get`/`cat`/`ls`/`share` start an in-process node for the command; foreground `nu share` serves until interrupted and says content is available only while a holder is online.
- `nu status` gains connectivity and share sections; `nu doctor` basic diagnostics (peer unreachable, no addresses, UDP blocked) before the full NAT doctor (0024).
- Implement resolved 0035 API Host/Origin/CSRF/DNS-rebinding defenses and gateway origin isolation (`<cid>.localhost` subdomains where verified, non-active fallback otherwise). Private gateway exposure is explicit, not accidentally protected by libp2p gating. Safe extraction for `get -o`.

**Acceptance**
- `nutest` and two real LAN/routed machines (no NAT between them): add → share → get of a file and a directory, including a recipient with no daemon and no prior `init`.
- The gateway serves a static site from a tree CID under the 0035 isolation rules.
- Fresh Linux/macOS users complete account-free add/share/get/local-browser journeys without reading protocol docs; setup steps/time and confusing errors recorded in docs/validation.md. Public exposure is concise and mode-aware, not repetitive confirmation ceremony.
- Malicious content pages cannot operate the API; unsafe extracted paths/symlinks are rejected under 0035.

## Acceptance criteria

## Work log
