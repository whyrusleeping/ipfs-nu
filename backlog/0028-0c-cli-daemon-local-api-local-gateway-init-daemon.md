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
spec_refs:
    - 4. User experience
    - 10. Local API, gateway, browsers
---

## Description
The user-facing Phase 0 surface: the go-ipfs-style experience (§4), no identity needed.

- `nu init` (node key, `~/.nu`, config), `nu daemon` (host + relays + lookup + DHT + store).
- Localhost API on :5171: versioned JSON over HTTP with an OpenAPI description; loopback only unless a token is set. **No Kubo API compatibility.**
- CLI (a thin client over the API): `add` (wrapped file nodes by default, `--raw`, `-r` for directories, `--nocopy`, `--no-announce`), `cat`, `get`, `ls`, `pin add|rm|ls`, `share` (link with PeerId + relay hints), `doctor`.
- Local gateway on :8171: `/tree/<cid>/<path>`, `/bytes/<cid>`, file nodes served with name/mime, directory listings paged by name, subdomain mode.

**Acceptance**
- End-to-end on one machine and between two NAT-lab nodes: add → share → get of a file and a directory.
- The gateway serves a static site from a tree CID.
- `nu doctor` output is reachable from the CLI.

## Acceptance criteria

## Work log
