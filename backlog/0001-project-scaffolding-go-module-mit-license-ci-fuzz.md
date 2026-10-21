---
id: "0001"
title: 'Project scaffolding: Go module, MIT license, CI, fuzz + Linux/macOS builds'
status: in_progress
priority: 1
created: "2026-09-24"
updated: "2026-10-21"
depends_on: []
spec_refs:
    - D18. Implementation layout
    - docs/networking.md#D16. Testing and diagnostics
---

## Description
Set up the repository skeleton everything else builds on.

- Go module `github.com/whyrusleeping/nu` (decided, recorded in D18) with the D18 package layout (empty packages with doc comments). No Go `internal/` directories. A separate `experiments/` Go module for spikes (0037, 0038) that may use go-libp2p/Kubo and is never imported by the main module.
- Product name in one constant + `cmd/nu`, `NU_` env prefix (the name is provisional; see spec open decisions).
- MIT `LICENSE`; `NOTICE` convention for code adapted from Tailscale (BSD-3).
- CI: `go vet`, staticcheck, `go test ./...` (fuzz seed corpora run as ordinary tests). `-race` on the same fast suite. Fuzzing campaigns are 0068, not PR CI.
- No-cgo builds (`CGO_ENABLED=0`) for linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64. Linux binaries are static; native macOS tests and signing/packaging are distinct requirements, not a claim of static macOS linking.
- Linux and one macOS test job; more native architectures are 0068. Linux netns remains the NAT/isolated server harness (0003, not v0). Distribution/recovery task 0044 owns the user install/service/upgrade workflow.
- Dependency policy check: CI fails if `github.com/libp2p/go-libp2p` (or `tailscale.com`) ever appears in the main module's `go.mod`.
- CI wiring for the conventions in CONTRIBUTING.md: `go test ./...` without root; `natlab` build-tagged jobs separate; `testdata/vectors/` layout.

**Acceptance (v0 cut; deferred evidence is in 0068)**
- All four OS/architecture targets cross-compile; CI runs `go vet`, staticcheck and `go test ./...` on linux/amd64 and one macOS runner on every PR, in well under a minute at this stage.
- Fuzz template: one example fuzz target whose seed corpus runs as part of `go test`; no fuzzing campaign in PR CI.
- `NU_SLOW=1` opt-in convention for slow tests (see CONTRIBUTING.md) wired into an on-demand CI job.
- Dependency guard fails a PR that adds go-libp2p.

## Acceptance criteria

## Work log
- 2026-10-21: Scaffold committed (da0db55). Verified locally: `go vet`, staticcheck 0.8.2, `go test ./...` and `-race` green in ~1 s; all four targets cross-compile with CGO_ENABLED=0 and Linux binaries are static; `scripts/depguard.sh` rejects a go.mod naming go-libp2p. Fuzz template is `core/varint` (seed corpus runs in `go test`); `nutest/slow.Skip` gates NU_SLOW tests; `.github/workflows/slow.yml` is the on-demand job. The repo has no GitHub remote yet, so the workflows (Linux + macOS runners) have not run on GitHub; the first push will exercise them.
