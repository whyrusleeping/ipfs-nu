---
id: "0065"
title: 'Local walking skeleton: auto-init, daemon, API conventions, client, local CLI and loopback gateway'
status: todo
priority: 1
created: "2026-10-21"
updated: "2026-10-21"
depends_on:
    - "0009"
spec_refs:
    - Local API, gateway and browsers
    - User experience
    - D18. Implementation layout
---

## Description
The first end-to-end runnable product, with no networking: everything later plugs into it instead of being built in isolation. Split from 0028, which keeps the networked share/get journey.

- `cmd/nu` binary; `$NU_PATH` (default `~/.nu`) with `config.toml`, node key, SQLite DB and blob dir; auto-init on first command; `nu init` optional; `nu config get|set`; product name in one constant.
- `node/` assembly and `nu daemon` (API :5171, gateway :8171, libp2p UDP :4171 reserved for later tasks).
- API skeleton per spec.md "Local API, gateway and browsers": versioned JSON + OpenAPI, the error envelope with stable codes → CLI exit categories, cursor paging with `resync`, and the **operation** resource (ID, target, stage, progress, NDJSON events, `wait`). Baseline loopback protections (Host must be a loopback name, no CORS, Origin rejection for state-changing requests); 0028 completes them per 0035.
- `client/` Go package; the CLI uses only it. `--json` on every command; TTY progress rendered from operation events.
- Local commands: `add` (file nodes, `--raw`, `--hash sha256`, `-r` with import cache, `--nocopy`), `cat`, `ls`, `pin add|rm|ls`, `dag get|put`, `car import|export`, `tree edit`, `status` (daemon/store summary; later tasks add sections), basic `doctor`.
- Target parsing per spec.md "Targets" (CIDs and paths now; names, datasets and links return a clear "not yet supported" error).
- Loopback gateway: `/tree/<cid>/…`, `/blob/<cid>`, file nodes with name/mime, directory listings with selected-root metadata, non-active fallback for active content.

**Acceptance**
- From a fresh machine (Linux and macOS): `nu add -r ./site` then browse it in the local gateway, with no prior `init`.
- Every command has `--json` output and documented exit codes; errors include an `action`.
- An operation started via API can be followed by `nu … --wait`, and its events stream as NDJSON.
- A 10 GB add/cat runs in constant memory with progress.

## Acceptance criteria

## Work log
