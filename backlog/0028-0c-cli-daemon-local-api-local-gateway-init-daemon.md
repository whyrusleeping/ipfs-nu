---
id: "0028"
title: '0c: CLI + daemon + local API + local gateway (init/daemon/add/cat/get/ls/pin/share)'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0027"
    - "0009"
    - "0035"
spec_refs:
    - User experience
    - Local API, gateway and browsers
---

## Description
First-magic preview surface: the go-ipfs-style account-free add/share/get/browser workflow. DHT and full public NAT sessions are optional integrations, not prerequisites for the direct/LAN slice.

- `nu init` (node key, `~/.nu`, config), `nu daemon` (host + store + explicit/LAN lookup; public relays/DHT integrate when available, disabled in private/isolated mode).
- Localhost API on :5171: versioned JSON over HTTP with an OpenAPI description; loopback only unless a token is set. **No Kubo API compatibility.**
- CLI (a thin client over the API): `add` (MASL file nodes by default, `--raw`, `--hash sha256`, `-r` for directories, `--nocopy`, `--no-announce`), `dag get|put` (atproto-style JSON), `car import|export` with walk profiles, `cat`, `get`, `ls`, `pin add|rm|ls`, `share` (link with PeerId + relay hints), `doctor`.
- Loopback gateway on :8171: `/tree/<cid>/<path>`, `/blob/<cid>`, `/.well-known/rasl/<cid>` (octet-stream, nosniff, stateless), file nodes served with name/mime, directory listings and selected-root metadata. Active content isolated by origin or safe non-active fallback; path routes alone do not settle security. Public HTTPS/browser sharing is 0043; conventional reads are not independent browser Bao verification.
- Implement resolved 0035 API Host/Origin/CSRF/DNS-rebinding defenses, stable JSON/errors and plain-language next actions. Private gateway exposure is explicit, not accidentally protected by libp2p gating.

**Acceptance**
- End-to-end on one machine and between two NAT-lab nodes: add → share → get of a file and a directory.
- The gateway serves a static site from a tree CID.
- Fresh Linux/macOS users complete account-free add/share/get/local-browser journeys without reading protocol docs; setup steps/time and confusing errors recorded in docs/validation.md. Public exposure is concise and mode-aware, not repetitive confirmation ceremony.
- `nu doctor` output is reachable from the CLI; basic diagnostics work before the full NAT doctor (0024), and later integrate it.
- Malicious content pages cannot operate the API; unsafe extracted paths/symlinks are rejected under 0035.

## Acceptance criteria

## Work log
