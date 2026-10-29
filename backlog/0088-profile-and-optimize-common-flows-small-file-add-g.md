---
id: "0088"
title: 'Profile and optimize common flows: small-file add/get, store point queries, mDNS storm'
status: done
priority: 3
created: "2026-10-29"
updated: "2026-10-29"
depends_on: []
spec_refs: []
---

## Description
User request (2026-10-29): run performance profiling and optimize common flows.

Done in this task:
- Opt-in profiling build: `go build -tags nuprof ./cmd/nu` + `NU_CPUPROFILE`/`NU_MEMPROFILE` (cmd/nu/prof.go).
- store: add Batch stages inline blobs and import-cache rows into its commit tx; `Store.PutMany` (one tx) used by the /nu/tree/1 client; prepared-statement cache for per-object queries; `temp_store=MEMORY`, 32 MiB page cache; schema 5 turns `blocks` into a rowid table (WITHOUT ROWID with ≤16 KiB rows read overflow pages on every lookup); migrations return freed pages/WAL; reclaim estimate updated.
- fetch: batched HasMany/GetMany instead of per-object checks after treeSync.
- extract: fewer os.Root calls per file.
- p2p/lookup: per-interface mDNS packet filtering, ≤1 query/s, refresh-only Remember saves at most once a minute, hoisted peer-ID encoding.
- node/swarm: interface IP listing reused for 1 s (identify).

Acceptance: before/after measurements recorded in docs/validation.md "Small-file add/get and mDNS profiling"; `go test ./...` passes.

## Acceptance criteria

## Work log
