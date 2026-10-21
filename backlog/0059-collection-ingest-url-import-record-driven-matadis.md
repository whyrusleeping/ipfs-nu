---
id: "0059"
title: Collection item push API and URL import
status: done
priority: 2
created: "2026-10-20"
updated: "2026-10-21"
depends_on:
    - "0057"
    - "0061"
    - "0035"
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
- 2026-10-21: Done on track/data (fb75bb3..126cd35; merged f8c5f7d + httpsrc dedupe fix). Push engine (durable accept queue = push log, ?wait=written, idempotence digests, conflict/replace, retries within ingest.limits, NDJSON batch, item DELETE, uploads), httpsrc.Import (resume, checksums), nu add https:// with provenance + --meta, producer listener with collection-scoped push tokens (nu api token …), ingest.hosts egress allowlist by network mode. Evidence: collection.TestItemPush (3 private members, 8 items to two receivers, stalled download + restarts + resend → same IDs duplicate:true, identical merged root, 8 items, 0 conflicts; refused unlisted host + redirect), natlab TestPrivateIngestEgress (nft counters: only ingest.hosts, no DNS), httpsrc.TestImportResumeAndChecksum, main.TestProducerPushCLI, node.TestAddURL. Real packet capture + real CollGS pilot → 0063. Deferred: ingest staging not charged to store.limit, producer TLS, per-producer rate limits, N39 test. Product questions raised: producer TLS, nu add URL under ingest.hosts, wildcard hosts, init tokens.
