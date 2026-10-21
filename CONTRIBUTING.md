# Contributing

Design lives in [spec.md](spec.md) and the docs it links. Work lives in `backlog/`. This file
covers how to build the code so many people and agents can work in parallel without drifting.

## Working on a task

- Take one actionable backlog task (all dependencies `done`). Work on its own branch or git
  worktree; do not share a working tree with another writer.
- The task's description and acceptance criteria are the contract. Read its `spec_refs`
  first. If acceptance cannot be met, say so in the task's work log rather than weakening it.
- Tasks that need a human decision (spending, operators, recruiting, accounts) are marked;
  do the agent-doable part and leave the decision explicit.
- Mark a task done only with the evidence its acceptance asks for (tests, vectors, recorded
  measurements in [docs/validation.md](docs/validation.md)).

## Decisions and the spec

- Many docs end with "Details still to specify". When a task needs one of them, decide it,
  write the result into the owning doc as concise normative text (no design history), and
  add test vectors where bytes are involved. Remove the item from the list.
- Do not silently invent behavior the spec marks as a feature gate (readiness evidence,
  safe release, security contracts). Implement the gate's conservative default instead.
- Each wire protocol gets a short message/limit/error description in its owning doc when
  it is implemented, precise enough for another implementation.

## Code

- Module `github.com/whyrusleeping/nu`, Go (latest stable), `CGO_ENABLED=0`. Layout is
  [D18](spec.md#d18-implementation-layout). No Go `internal/` directories.
- Dependencies: only those listed in D18 without a spec change. Never `go-libp2p` or
  `tailscale.com` in the main module (CI enforces this). Spikes that need them live in the
  separate `experiments/` module and are never imported.
- `core/` does no I/O. Parsers of untrusted input take explicit size/depth limits.
- Keep the product name in few places: `cmd/nu`, one constant for the binary/user-facing
  name, and the `NU_` environment prefix. The name is provisional.
- Logging uses `log/slog`. Prometheus metrics are prefixed `nu_` and use base units.
- Persistent formats are versioned from their first commit: SQLite schema version plus
  forward migrations; wire protocols by protocol ID; DRISL records by `$type`/`v`.

## API and CLI

Follow the conventions in [spec.md](spec.md#local-api-gateway-and-browsers): one error
envelope with stable codes and an `action` hint, `--json` everywhere, opaque cursors with
`resync`, and long-running work as operations with IDs, NDJSON events and `wait`. The CLI is
built only on the `client` package, so anything the CLI can do, the API can do. Errors say
what happened and what to do next, in plain language.

## Tests

- `go test ./...` must pass without root or network access.
- Fuzz every decoder of untrusted input (native Go fuzzing; CI runs a short smoke pass).
- Byte formats get vectors under `testdata/vectors/`, including ones we publish for other
  implementations. Cross-check against independent implementations where the task says so.
- Multi-node behavior (transfer, clusters, collections) is tested with `nutest`, the
  in-process harness: N nodes on loopback QUIC with temporary stores, restarts, partitions
  and clock skew.
- NAT and isolated-egress scenarios use the netns lab behind the `natlab` build tag; they
  need root and run in dedicated CI jobs.

## Commits

Small, focused commits with a short prefix naming the area, e.g. `store: …`, `p2p/host: …`,
`spec: …`, `backlog: …`. Spec changes that decide an open detail say so in the message.
