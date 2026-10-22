---
id: "0073"
title: 'Human/CI: native macOS validation, signing/notarization, root install on a real server (from 0044)'
status: todo
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on:
    - "0044"
spec_refs:
    - docs/operations.md#Authority, administration and recovery
    - Milestones
---

## Description
Carried from 0044 (agent-doable parts done; these need a Mac, Apple credentials or a real server). Preserves 0044's acceptance items that could not be met here:

- Native macOS arm64/amd64 runs of the release binaries; LaunchAgent install/upgrade path (`dist/install.sh`, `dist/macos/io.ipfs.nu.daemon.plist`).
- Record the APFS results of `node.TestFilenameRoundTrip` / `extract.TestExtractSafety` from the macOS CI job; APFS crash and sleep/wake tests for store and daemon.
- Signing + notarization: add secrets `MACOS_SIGN_P12_BASE64`, `MACOS_SIGN_P12_PASSWORD`, `MACOS_SIGN_IDENTITY`, `MACOS_NOTARY_APPLE_ID`, `MACOS_NOTARY_TEAM_ID`, `MACOS_NOTARY_PASSWORD` (via `ycc token set` / CI secrets, never in the repo) and run `.github/workflows/release.yml` on a tag.
- `install.sh --system` as root on a real Linux server, including upgrade with `--backup`.
- Independent security review of backup/restore and of the service-unit hardening.

Acceptance: results recorded in docs/validation.md (0044 section); macOS isolation claims stay separate from Linux netns results.

## Acceptance criteria

## Work log
