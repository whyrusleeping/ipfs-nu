---
id: "0049"
title: 'Production v1 acceptance: magic, versioned archives, reach, migration and operations'
status: todo
priority: 2
created: "2026-09-29"
updated: "2026-09-29"
depends_on:
    - "0029"
    - "0034"
    - "0039"
    - "0041"
    - "0042"
    - "0043"
    - "0044"
    - "0045"
    - "0046"
    - "0047"
    - "0048"
    - "0052"
    - "0053"
    - "0054"
    - "0055"
    - "0056"
spec_refs:
    - Milestones
    - docs/operations.md#Acceptance scenarios
    - docs/validation.md#Release evidence
---

## Description
Combined production gate, not a replacement for thin previews. Require completed repair/handoff (0048) and independent security review before passing this gate. Test fresh Linux/macOS sharing/browser UX plus actual team archive ingestion/history/restore. Publish validated support limits, install/upgrade/recovery runbooks, operated infrastructure/service quotas and verified IPFS import evidence before cutover recommendations.

Acceptance: all operations.md scenarios have repeatable evidence; exact version and policy waits honest under stale/insufficient evidence; real relayed/UDP-blocked endpoints and secure browser gateway tested; native macOS behavior recorded separately from Linux netns; migration restart/corruption/metadata/source-safety tests; independent networking/parser/application security findings resolved or explicit release blockers. No PB/NAT hypothesis promoted to support claim. Record results in docs/validation.md.

## Acceptance criteria

## Work log
