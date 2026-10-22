---
id: "0077"
title: 'Security fixes from internal review: public-listener codec swap, producer token local file read, public /c/ /nu/, cross-site GETs, P3 runtime, bounds'
status: in_progress
priority: 1
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/operations.md#API, gateway and public exposure
---

## Description
From the 2026-10-22 internal review (agent report, confirmed with real daemons). Fix all, each with a test and a negative-matrix row in docs/operations.md:

1. **Critical — codec swap on the public listener (P2):** `node/public.go` `PublicServes`/`walkPublic` key public objects by multihash only; `store.Store.Get` ignores the codec; `node/content.Resolve` decodes any bytes as DRISL. A published small raw file whose bytes are a MASL file node (or tree header/dir node) requested under the DRISL codec serves arbitrary held private content. Fix: key the public set by full CID (or compare requested vs stored codec via `Store.Has`), make `Store.Get(c)` fail on codec mismatch, audit other multihash-only gates (p2p serving).
2. **High — producer token reads local files:** the producer listener reuses `pushHandlers`; `files.<n>.path` reads any absolute path (node.key, tokens, ~/.ssh) into a replicated collection; errors reveal path existence; `upload` accepts any held raw CID. Fix: remote pushes cannot use `path`/`nocopy` (only the loopback API may), uploads accepted only if uploaded under this token/collection; document in api.md.
3. **Medium — `/c/` and `/nu/` on the public listener (P2/P4):** reveal dataset roots (X-Nu-Root) and trigger live name resolution for anonymous clients. Fix: 404 when `h.pub != nil` (like `at()`).
4. **Medium — cross-site GETs do work (A3):** `checkOrigin` skips GET/HEAD; `/cat`, `/ls`, `/resolve` fetch/resolve/store. Fix: apply Sec-Fetch-Site/foreign-Origin checks to every method; update N8 and the A-rules.
5. **Medium — P3 only checked at daemon start:** `ClusterInit`/`ClusterJoin` into private/isolated mode while the public listener runs. Fix: refuse (action names `public.listen`) or stop the listener.
6. **Low — public listener resource bounds:** cap Range parts (>16 → ignore Range), cap/paginate listings, write deadlines/minimum rate for slow readers.
7. **Low — extraction:** X1 all-or-nothing (pre-validate the walk, backslash names) and TOCTOU — write via `os.OpenRoot(dest)`.
8. **Low — `.`/`..` on bare-origin paths redirect instead of 400 (G8/N15):** validate before `ServeMux`.
9. **Low — listings lack ETag (G9).**
10. **Info:** add `Cross-Origin-Resource-Policy: same-site` on loopback bare-origin responses (embedding probe); runbook note on cookie scope for `<cid>.<domain>` unless the domain is on the PSL.

Acceptance: every item fixed with a regression test (the critical/high ones reproduce the reviewer's exploit and now fail), matrix rows added, `go test ./...` stays fast.

## Acceptance criteria

## Work log
