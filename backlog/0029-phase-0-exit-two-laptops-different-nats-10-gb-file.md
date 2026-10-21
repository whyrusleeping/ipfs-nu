---
id: "0029"
title: 'First magic preview exit: different NATs, 10 GB/10k-file tree, browser and 3-peer download'
status: todo
priority: 2
created: "2026-09-24"
updated: "2026-10-21"
depends_on:
    - "0028"
    - "0020"
    - "0003"
    - "0011"
spec_refs:
    - Milestones
---

## Description
The public different-NAT first-magic preview gate, distinct from earlier direct/LAN CLI completion and later production/archival gates. Run NAT-lab, real-network and fresh-user demonstrations. Hard-NAT optimization and global DHT are not prerequisites; relay integration is.

**Scenarios**
1. Two "laptops" behind different NATs (cone ↔ cone, and cone ↔ symmetric once 0b+ lands), with **no configuration**, share a **10 GB file** and a **10k-file tree** via `nu share` links.
2. A verified download of the 10 GB file from **3 peers** at once.
3. A transfer preserves verified progress across a Wi-Fi → hotspot style network change. Inner-connection preservation is the separately measured Nu-path goal; resumable progress is the product requirement.
4. A same-file-same-CID check: adding identical bytes on two machines gives identical blob CIDs, and the file-node CIDs match when name/mime match.
5. Wire-compat sanity: the node still passes the 0a interop suite with all features on.

**Acceptance**
- All scenarios pass in CI (NAT lab) and are demonstrated once on real networks (two home networks or home + phone hotspot).
- Fresh users browse shared content through the secured local gateway; public HTTPS browser serving is tested separately in 0043. Account-free setup and errors need usability evidence, not only expert CI demonstrations.
- Record reproducible results, environments, limitations and failures in docs/validation.md. Different-NAT testing uses declared admitted development relays; broad default-service claims require 0039 and production v1 gate 0049.

## Acceptance criteria

## Work log
