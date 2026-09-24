---
id: "0029"
title: 'Phase 0 exit: two laptops, different NATs, 10 GB file + 10k-file tree, 3-peer download'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-09-24"
depends_on:
    - "0028"
    - "0024"
    - "0011"
spec_refs:
    - 18. Phasing
    - 14. Rough numbers (targets to measure)
---

## Description
The end-to-end proof that Phase 0 works, run in the NAT lab (and once manually on real networks).

**Scenarios**
1. Two "laptops" behind different NATs (cone ↔ cone, and cone ↔ symmetric once 0b+ lands), with **no configuration**, share a **10 GB file** and a **10k-file tree** via `nu share` links.
2. A verified download of the 10 GB file from **3 peers** at once.
3. A transfer continues across a Wi-Fi → hotspot style network change.
4. A same-file-same-CID check: adding identical bytes on two machines gives identical blob CIDs, and the file-node CIDs match when name/mime match.
5. Wire-compat sanity: the node still passes the 0a interop suite with all features on.

**Acceptance**
- All scenarios pass in CI (NAT lab) and are demonstrated once on real networks (two home networks or home + phone hotspot).
- Results are recorded in the spec's rough-numbers section (§14).

## Acceptance criteria

## Work log
