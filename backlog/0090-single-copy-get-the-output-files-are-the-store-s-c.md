---
id: "0090"
title: 'Single-copy get: the output files are the store''s copy (torrent-style)'
status: proposed
priority: 3
created: "2026-10-29"
updated: "2026-10-29"
depends_on: []
spec_refs:
    - docs/data.md#D17. Store and replication
    - docs/operations.md#X. Extraction (`nu get -o`)
---

## Description
**Idea (from 0089).** `nu get -o out` currently writes every byte twice: once into the repo (pinned, served to peers, resume state) and once into the output, both usually on the same disk. Torrent clients keep one copy: they verify pieces in the downloaded files and seed from those. Proposal: a get downloads straight into the output, and the store keeps only the Bao outboard plus path, size and mtime, as a `--nocopy` add does today. The node then seeds from the output.

**Trade-offs to decide:**
- If the user edits, moves or deletes the output, the node stops holding that content. The existing `--nocopy` size/mtime check re-hashes it and drops the row, and the pin goes incomplete. The repo is then no longer an independent copy; the spec already calls `--nocopy` "a mutable local source, not an independent archive copy".
- Choose the default: single copy, with `--copy` to keep a separate repo copy, or the reverse.
- Download state (the verified-group bitmap and outboard) is kept for a sparse file at the output's `.nu-partial` path, not under `partial/`. Resume and GC must understand it.
- The top-level rename (X7) must move the references with it: register the rows only after the rename, or update their paths.
- Small blobs (≤ 16 KiB inline, ≤ 1 MiB without an outboard) and SHA-256 blobs need a rule. Small files could simply stay copied.
- An existing output updated in place: references to files that are later replaced.

**Expected gain (0089 measurements on alphac):** the second write is ~0.3 s of a 2:1 10 GiB fetch on top of its checkpoint cost, half the disk writes, and half the repo space for gets. It matters more on slower disks and for small-file trees (one fewer synced file per file).

**Acceptance (draft):** a get of a 10 GiB file and of a 10k-file tree writes each byte once (measured by bytes written to the device); the result is served to a second peer from the output; editing an output file makes it unheld without serving bad data; resume after a kill works; spec D17 and X7 updated.

## Acceptance criteria

## Work log
