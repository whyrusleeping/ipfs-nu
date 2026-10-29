---
id: "0091"
title: 'get: reflink the output from the store on copy-on-write filesystems'
status: proposed
priority: 4
created: "2026-10-29"
updated: "2026-10-29"
depends_on: []
spec_refs:
    - docs/operations.md#X. Extraction (`nu get -o`)
---

## Description
**Idea (from 0089).** Keep the two copies `get` makes (repo plus output), but make the output share the store's blocks where the filesystem can, so it costs no data write:
- Linux `FICLONE` / `copy_file_range` on btrfs and XFS (reflink);
- macOS `clonefile` on APFS.

ext4 has no reflink and keeps today's copy, possibly via `copy_file_range` to avoid the userspace copy.

**Notes:**
- A whole-file clone only works once the blob is complete. The streaming path (X7) copies groups as they verify, so a clone either replaces the copy for blobs already held when extraction reaches them, or the copy is redone as a clone at commit. Measure both.
- The clone must come from the committed blob file (`blobs/…`), after the rename from `partial/`.
- No `cgo`: use `golang.org/x/sys/unix` ioctls, which needs a D18 dependency check; the stdlib `copy_file_range` path in `os.File.ReadFrom` needs an `*os.File` source.
- This complements the single-copy proposal: it keeps the repo an independent copy, but only helps on CoW filesystems.

**Acceptance (draft):** on btrfs or XFS (and APFS when 0073's macOS runs exist), `nu get` of a 10 GiB file writes ~10 GiB to the device, not ~20 GiB; on ext4 there is no change in behavior or speed; outputs verify; X7 documented.

## Acceptance criteria

## Work log
