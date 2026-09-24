---
id: "0001"
title: 'Project scaffolding: Go module, MIT license, CI, fuzz + Linux static builds'
status: todo
priority: 1
created: "2026-09-24"
updated: "2026-09-24"
depends_on: []
spec_refs:
    - D18. Code layout (Go)
    - D16. "Just works" is an engineering requirement
---

## Description
Set up the repository skeleton everything else builds on.

- Go module with the D18 package layout (empty packages with doc comments). Module path: pick one (suggest a vanity path under nu.ipfs.io, e.g. `nu.ipfs.io/nu`); record the choice in the spec.
- MIT `LICENSE`; `NOTICE` convention for code adapted from Tailscale (BSD-3).
- CI: `go vet`, staticcheck/golangci-lint, `go test -race`, native Go fuzzing smoke run on every PR (longer fuzz runs nightly).
- Static builds (`CGO_ENABLED=0`) for linux/amd64 and linux/arm64.
- Dependency policy check: CI fails if `github.com/libp2p/go-libp2p` (or `tailscale.com`) ever appears in `go.mod`.

**Acceptance**
- `go build ./...` and `go test ./...` pass in CI on both architectures.
- Fuzz harness template exists and runs in CI.
- Dependency guard fails a PR that adds go-libp2p.

## Acceptance criteria

## Work log
