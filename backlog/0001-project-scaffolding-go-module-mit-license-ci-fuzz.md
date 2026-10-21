---
id: "0001"
title: 'Project scaffolding: Go module, MIT license, CI, fuzz + Linux/macOS builds'
status: todo
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
- CI: `go vet`, staticcheck/golangci-lint, `go test -race`, native Go fuzzing smoke run on every PR (longer fuzz runs nightly).
- No-cgo builds (`CGO_ENABLED=0`) for linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64. Linux binaries are static; native macOS tests and signing/packaging are distinct requirements, not a claim of static macOS linking.
- Native Linux/macOS test jobs for workstation paths; Linux netns remains the NAT/isolated server harness. Distribution/recovery task 0044 owns the user install/service/upgrade workflow.
- Dependency policy check: CI fails if `github.com/libp2p/go-libp2p` (or `tailscale.com`) ever appears in the main module's `go.mod`.
- CI wiring for the conventions in CONTRIBUTING.md: `go test ./...` without root; `natlab` build-tagged jobs separate; `testdata/vectors/` layout.

**Acceptance**
- All four OS/architecture targets build; native Linux/macOS test coverage is recorded, with any unavailable native architecture runner explicit. Cross-build success alone is not portability evidence.
- Fuzz harness template exists and runs in CI.
- Dependency guard fails a PR that adds go-libp2p.

## Acceptance criteria

## Work log
