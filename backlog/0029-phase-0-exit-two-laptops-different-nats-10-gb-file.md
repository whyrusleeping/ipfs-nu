---
id: "0029"
title: 'First magic preview exit: different NATs, 10 GB/10k-file tree, browser and 3-peer download'
status: in_review
priority: 2
created: "2026-09-24"
updated: "2026-10-22"
depends_on:
    - "0068"
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
- 2026-10-22: Automated NAT-lab exit (c1c5453, 5542b77; merged f3335c2): NU_SLOW=1 ./scripts/magic-preview.sh. PASS: cone↔cone and cone↔symmetric (relay) 10 GB + 10k-file trees, 3-provider verified 10 GB download, same-bytes-same-CID, full-daemon interop 6/6 + Kubo. Scenario 3 initially FAILED (network switch killed the get; both daemons needed restarts) — addressed by 0024's fixes (transient send errors, immediate re-probe, re-identify) whose real-daemon hotspot-switch-mid-get test now passes without restarts; re-run the 0029 512 MiB network-change scenario to confirm. Pending human: real home-network/hotspot demo, fresh-user browsing/usability evidence, observing CI remotely.
- 2026-10-22: Scenario 3 now PASSES (034772d; merged ba2e96a): TestMagicPreviewMovement — recipient-only, provider-only and simultaneous interface/subnet/WAN switches from cone networks to APDM/APDF hotspots; three 512 MiB gets complete without daemon restart or CLI retry (7.2–8.8 s after the switch), digests verified; restart workaround removed from the full preview test. All automated scenarios 1–5 pass. Remaining (human): real home-network/hotspot demonstration, fresh-user account-free browsing/usability evidence (checklist in docs/validation.md), observing the natlab CI job remotely.
