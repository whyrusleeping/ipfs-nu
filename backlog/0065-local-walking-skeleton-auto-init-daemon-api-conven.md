---
id: "0065"
title: 'Local walking skeleton: auto-init, daemon, API conventions, client, local CLI and loopback gateway'
status: in_progress
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
- Local commands: `add` (file nodes, `--raw`, `--hash sha256`, `-r` with import cache, `--nocopy`), `cat`, `ls`, `pin add|rm|ls`, `dag get|put`, `status` (daemon/store summary; later tasks add sections), basic `doctor`.
- Not in v0: `car import|export` (lands with 0051/0053 walk profiles) and `tree edit` (with 0045/0047 publishing).
- Target parsing per spec.md "Targets" (CIDs and paths now; names, datasets and links return a clear "not yet supported" error).
- Loopback gateway: `/tree/<cid>/…`, `/blob/<cid>`, file nodes with name/mime, directory listings with selected-root metadata, non-active fallback for active content.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- From a fresh machine: `nu add -r ./site`, then browse it in the local gateway, with no prior `init` (checked by an end-to-end test on CI's Linux and macOS runners).
- Every command has `--json` output and documented exit codes; errors include an `action`.
- An operation started via API can be followed by `nu … --wait`, and its events stream as NDJSON.
- Add/cat stream in bounded memory with progress (64 MiB test; multi-GB under `NU_SLOW`).

## Acceptance criteria

## Work log
- 2026-10-21: Store-independent groundwork (NU_PATH/config/auto-init, API conventions + operations, client, CLI framework, daemon shell, store seam) started early on branch track/app, since only add/cat/ls/pin/dag need 0009.
- 2026-10-21: Done on track/app (merged). Evidence: cmd/nu TestAddSiteAndBrowse (fresh NU_PATH, daemonless add -r, gateway <cid>.localhost index.html + /tree listing), TestCLIFollowsOperationAndExitCodes (--json, exit codes, actions), node/daemon e2e (API-started add, wait, NDJSON events, cat), node TestAddCat64MiB (48 MiB heap bound, progress) + TestAddCatMultiGB (NU_SLOW), TestAddCatLsDagPins. Manually checked with the built binary. CI macOS run pending a GitHub remote. Decisions in docs/api.md (no MASL content-type, dag put unpinned, import cache keyed by path/hash/size/mtime/inode).
