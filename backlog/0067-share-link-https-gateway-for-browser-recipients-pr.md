---
id: "0067"
title: Share-link HTTPS gateway for browser recipients (proposal)
status: done
priority: 3
created: "2026-10-21"
updated: "2026-10-22"
depends_on:
    - "0028"
    - "0039"
spec_refs: []
---

## Description
Proposal (spec.md "Open decisions": public infrastructure). Today a browser recipient needs the sharer to run a TLS seed. A project-operated gateway for share links only would restore early IPFS's "open the link in a browser" moment without hosting obligations:

- `https://<share-gateway>/<link>` streams verified content from the sharer's node (via the link's hints) while it is online; no caching, no fetching of arbitrary CIDs.
- Download-only on a sandbox domain (Content-Disposition attachment, nosniff, no rendering of active content), size/time/bandwidth caps, abuse reporting and takedown.
- `nu share` prints it only when the user opts into public sharing.

Open questions: abuse (malware/phishing distribution under a project domain), relayed bandwidth cost, operator and legal responsibility. Needs user acceptance and 0039's operator/funding decision.

## Acceptance criteria

## Work log
- 2026-10-22: REJECTED by the user (not necessary). Closed without implementation; browser recipients use an operator's own public seed (0043).
