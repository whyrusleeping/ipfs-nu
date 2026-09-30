---
id: "0055"
title: 'MASL bundles: pin, isolated-origin rendering with header allowlist, tree → bundle/.tile export'
status: todo
priority: 3
created: "2026-09-30"
updated: "2026-09-30"
depends_on:
    - "0043"
    - "0028"
spec_refs:
    - docs/dasl.md#MASL
---

## Description
MASL beyond single-file nodes (D21).

- Recognise bundle-mode MASL documents: a recursive pin follows every `resources/*/src`. The gateway serves `/` as the default, matches exact paths (no implied directories), ignores query strings, and renders only on isolated origins.
- The MASL header allowlist (`content-type`, `content-language`, `content-disposition`, `content-encoding`) is validated. CSP may only tighten policy. `sourcemap`/`speculation-rules`/`service-worker-allowed`/manifest fields are ignored for now.
- `nu masl bundle <tree-cid>[/path]` exports a bundle when it fits the DRISL limit, optionally as a `.tile` CAR (MASL header plus resources). It is core DASL only when the blobs were added with `--hash sha256`.

**Acceptance**
- Round-trip with @dasl/tiles (load a Nu-exported `.tile`; pin and serve a tiles-produced bundle).
- Header-injection, source-map exfiltration and same-origin escape tests pass.

## Acceptance criteria

## Work log
