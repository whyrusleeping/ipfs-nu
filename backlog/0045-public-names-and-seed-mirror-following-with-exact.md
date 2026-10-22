---
id: "0045"
title: Public names and seed/mirror following with exact-target readiness
status: in_progress
priority: 2
created: "2026-09-29"
updated: "2026-10-22"
depends_on:
    - "0028"
    - "0035"
    - "0036"
    - "0064"
    - "0051"
spec_refs:
    - docs/names.md#Identity and names
    - docs/follow.md#Following datasets
    - docs/operations.md#Reader-visible versions
---

## Description
**First step (moved from 0035):** specify OAuth/identity discovery security in docs/names.md: SSRF controls on handle/DID/PDS/authorization-server discovery (via `httpsrc`), malicious endpoint validation, token storage and scope (`io.ipfs.nu.authPublish`). Follows are mirror-preset datasets in the local cluster with a `name` source (docs/follow.md), sharing dataset status/listing.

Implement atproto OAuth scoped login, node registration, tree/file/blob name records, publish/resolve with proofs/cache and seed follow. Full/scoped/lazy modes, observed/syncing/held state, held-view atomic switches, exact-target seed waits, resource budgets and actionable lag/deletion/unfollow lifecycle use shared store/transfer. Initial polling/proof paths first; peer watch/swarm/Jetstream scalability extends same model, not new sync stack. DID-wide form ships only after explicit future-name/non-tree/aggregate-quota rules.

Acceptance: publish + seed-completion wait + laptop sleep + fresh third retrieval of acknowledged CID; stale cached resolution without name-plane requests is labeled; crash switches and quota stops preserve held view; named get binds one CID while follow gateway exposes held root; orphan pins visible; mocked and real OAuth/PDS tests. Explicit held/offline read selection differs from default resolved-target get and always exposes selected CID. No public-name history, rollback-prevention or publication-as-readiness promise. Account-free seed availability is fixed-CID cluster/client pin with content wait; outsider sharing requires public mode and explicit announce/public HTTPS consent. Private pins remain members-only. Named publication requires atproto login.

## Acceptance criteria

## Work log
- 2026-10-22: Done for its scope (1fdab45..112296e; merged fa2b6ca). Own atproto OAuth client (PAR/PKCE/DPoP, loopback callback, refresh/revoke) over httpsrc; sessions 0600; names/ records + resolver with cache/freshness; publish/resolve/names/unpublish, node register/devices; name targets in get/cat/ls and gateway /nu/…; follows as mirror datasets with observed/syncing/held roots and states; publish --wait-seed (seed = cluster storage member, replicas: all); opt-in Jetstream hint source limited to followed DIDs (re-resolution only, no ingest; off by default). Evidence: nutest.TestPublishSeedWait (R2, R19, W1/W4/W5), node.TestNamesPublishResolveFollow, cmd/nu.TestNamesCLI, atproto.TestOAuthLoginAndWrite, node.TestFollowJetstreamHint, NU_LIVE TestLiveResolveAndDiscovery against bsky.social. Not built: scoped/lazy follows (refused), DID-wide follows, /nu/name/1 push, swarm key, transfer budgets, publisher-node provider hints, publish coalescing, non-member seed waits. Product questions raised: OAuth client_id hosting, scopes vs permission-set lexicon, seed layout default, Jetstream default.
