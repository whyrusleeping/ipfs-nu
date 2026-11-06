---
id: "0096"
title: Linux server `install.sh --system` validation as root (from 0073)
status: done
priority: 2
created: "2026-11-06"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/operations.md#Authority, administration and recovery
---

## Description

Split from 0073: the only part of 0073 doable without a Mac, Apple credentials or an independent human reviewer. Run `dist/install.sh --system` as root on a real Linux server (alphac-3: passwordless sudo), install + upgrade with `--backup`, a real workload (add, remote LAN fetch, gateway), crash restart, then remove it.

Acceptance: results recorded in docs/validation.md (0044 section) and the item removed from its "Human or CI only" list; bugs found in the unit/installer fixed and re-verified on the server.

## Outcome

install.sh --system validated as root on alphac-3 (Ubuntu 26.04/systemd 259): install, two upgrades with --backup (0600 backups restore as the same peer), crash restart, 2 GiB LAN fetch byte-exact, gateway, then removed. Found and fixed: PrivateTmp=yes made `nu get -o /tmp/x` report success into the daemon's private /tmp and `nu add /tmp/f` fail; the unit now shares host /tmp (re-verified). Recorded in docs/validation.md (0044) and operations.md I3; 0073 blocked on Mac/Apple creds/human security review.

Commit: dist, docs, backlog: root `install.sh --system` validated on a real server; system unit drops PrivateTmp (0096, split from 0073)
