---
id: "0059"
title: 'Collection item push API and URL import'
status: todo
priority: 2
created: "2026-10-20"
updated: "2026-10-20"
depends_on:
    - "0057"
    - "0061"
spec_refs:
    - docs/collections.md#Ingest
    - docs/data.md#D17. Store and replication
    - docs/validation.md#Pilot candidates
---

## Description
Implement the producer push path per docs/collections.md#Ingest and URL import per docs/data.md#D17. Producers (e.g. vmx's torrentizer) do their own source discovery (Matadisco/Jetstream, STAC) and transforms; Nu does no feed consumption.

Scope: `nu add https://…` URL import (hash on arrival, Range/If-Range resume, provenance, SSRF/redirect guards, sha256 8 MiB limit only); `POST /v1/cluster/collections/<ds>/items` and `nu cluster collection put` with uploads, local paths (`--nocopy`) or URLs with optional checksums; durable accept queue, `?wait=written`, idempotence by path, conflict + `replace`; retries within `ingest.limits`, failed state, push log with cursor; NDJSON batch; item DELETE; collection-scoped API credentials; private-mode `ingest.hosts` egress allowlist on receiving members.

Acceptance: CollGS pilot push criterion (torrentizer pushes items instead of building torrents; producer restarts/resends cause no lost or duplicated items; pushes to two receiving members merge with no conflicts for deterministic items); SSRF/redirect/egress tests incl. private-mode packet capture showing only listed hosts; resume and checksum-mismatch tests.

## Acceptance criteria

## Work log
