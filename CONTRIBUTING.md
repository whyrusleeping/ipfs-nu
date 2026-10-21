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
  measurements in [docs/validation.md](docs/validation.md)), and no more: evidence a task
  does not ask for belongs to another task.
- Before v0, prefer the simplest implementation that meets the task and leaves the formats
  right. Optimization, extra configurability and hardening wait until v0 works end to end.

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

## Tests: fast, few, and aimed at v0

The current goal is **v0** ([milestones](spec.md#milestones)): a working add/share/get/browse
between two machines, as soon as possible. Iteration speed beats coverage until then. Code
will churn; tests that pin down internals make every change slower.

**What to test**
- The task's acceptance criteria, one test each, at the package's public boundary.
- Byte formats and hashes (CIDs, DRISL, MST nodes, Bao, wire messages): vectors under
  `testdata/vectors/`. These are permanent contracts, so they are worth it.
- Verification and safety logic: corrupt data is rejected, unpinned-only GC, atomic writes.
- Multi-node behavior through `nutest`, the in-process harness, with a few end-to-end tests
  rather than many small ones.

**What not to test**
- Plumbing, getters, flag parsing, log lines or error-message wording.
- Hypothetical edge cases nobody asked for, or every permutation of a table that already
  covers its classes.
- The same behavior at several layers. Pick the one where it is cheapest to observe.

**Speed budget**
- `go test ./...` runs without root or network and stays under about a minute in total.
  Each test should take well under a second.
- Anything slower (multi-GB files, long property runs, soak, benchmarks) is opt-in: skip
  unless `NU_SLOW=1` (call `slow.Skip(t)` from `nutest/slow`). Keep the default sizes small (MiB, not GiB) and the iterations bounded.
- No `time.Sleep` waits. Wait on conditions with timeouts.
- Fuzz targets are written for decoders of network or file input, but only their seed
  corpora run in `go test` (template: `core/varint`). Fuzzing campaigns, benchmarks, cross-implementation checks and
  scale tests deferred from v0 live in backlog task 0068.

**While iterating**
- Run only the tests of packages you changed (`go test ./store/...`), plus direct dependents
  if you changed an API. Run `go test ./...` once before finishing a task.
- Do not run `NU_SLOW`, `natlab`, interop or benchmark suites unless your task is about
  them. Big evaluation runs belong to the exit tasks (0011, 0024, 0029, 0063, 0049, 0068),
  not to feature tasks.
- If an existing test is slow or flaky, fix or delete it rather than routing around it.

NAT and isolated-egress scenarios use the netns lab behind the `natlab` build tag; they need
root and run in dedicated CI jobs. Wire interop with other libp2p implementations lives in
the separate `experiments/` module (a go-libp2p smoke test per protocol before v0) and 0011.

## Commits

Small, focused commits with a short prefix naming the area, e.g. `store: …`, `p2p/host: …`,
`spec: …`, `backlog: …`. Spec changes that decide an open detail say so in the message.
