---
id: "0097"
title: Software NAT-lab per-pair success table, release gate, EIM↔random-APDM ≥98% (split from 0025)
status: done
priority: 3
created: "2026-11-06"
updated: "2026-11-06"
depends_on: []
spec_refs:
    - docs/networking.md#D11c. NAT traversal and relay optimization
---

## Description

Agent-doable part of 0025 (the hardware lab needs physical routers/hotspots and stays in 0025).

- Generate a per-pair success table from the rootless netns NAT lab: lab tests emit pair outcome records (`NU_NATLAB_RESULTS` ndjson), a small tool aggregates them into a markdown table with each pair's expectation class and exits non-zero on regressions.
- Rows: every lab pair where at least one side is EIM, public, router-mapped or IPv6 must be direct; EIM↔random-APDM (EIF and APDF) must be ≥ 98 % over a stated trial count; APDM↔APDM rows are reported (relay expected), not gated.
- The release workflow runs it, attaches the table to the release, and fails (blocks publish) on a regression; the current table is published in docs.

Acceptance (0025's lab-side criteria, software NATs only):
- Direct for every lab pair where at least one side is EIM, public, mapped or IPv6.
- ≥ 98 % for EIM↔random-EDM in the lab campaign.
- Table published; regressions fail the release checklist.

## Outcome

TestHardNATLab is the full software-lab pair matrix (public, EIM/EIF, EIM/APDF, sequential/random APDM, IPS row) recording natlab.Record pair outcomes; public/IPv6/mapped tests record too. p2p/natlab/cmd/nattable renders and gates the table (always-direct for EIM/public/mapped/IPv6 pairs, ≥98% over a campaign only for EIM↔random-APDM, missing rows fail); scripts/nat-table.sh runs it; release.yml nat-table job blocks publish and attaches nat-pairs.md. Random search raised 1024→2048 ports (analytic miss 1.7%→0.03%). Local campaign-100 run: every gated row passes, EIM/APDF↔random 100/100; table published in docs/validation.md. Remote release-workflow run pending.

Commit: disco, natlab, scripts, ci, docs: software NAT-lab per-pair success table with release gate; hard-NAT random search 2048 ports (0097, split from 0025)
