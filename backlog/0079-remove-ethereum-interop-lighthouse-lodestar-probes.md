---
id: "0079"
title: 'Remove Ethereum interop: Lighthouse/Lodestar probes, eth2 ping, CI job, doc claims'
status: in_progress
priority: 2
created: "2026-10-22"
updated: "2026-10-22"
depends_on: []
spec_refs:
    - docs/networking.md#D8. Wire profile and compatibility
    - Goals and scope
---

## Description
Ethereum interoperability is explicitly out of scope and unwanted (user, 2026-10-22). It entered via task 0011, which turned an aside ("even ethereum uses libp2p") into an acceptance criterion. libp2p interop targets are go-, rust- and js-libp2p and Kubo over QUIC only.

Remove:
- `interop/main.go`: the `eth2` ping mode (`/eth2/beacon_chain/req/ping/1/ssz_snappy`, `ethereumPing`) and the `github.com/golang/snappy` dependency from `interop/go.mod`/`go.sum`.
- `interop/run.py`: the Lighthouse and Lodestar images, the client choices and the eth2 handling; the `blackbox` default becomes Kubo only.
- `interop/README.md`: the Lighthouse/Lodestar sections; `interop/results/` Ethereum result directories (e.g. `lodestar-final/`, `lodestar-js-control/`, Lighthouse outputs) if present.
- `.github/workflows/interop.yml`: rename the "Kubo and Lighthouse" job to Kubo.
- `docs/networking.md`: "Ethereum consensus clients with QUIC endpoints" from the interop targets (around line 75), the Ethereum consensus-spec reference link and any other Ethereum mention.
- `docs/validation.md`: the Lighthouse/Lodestar evidence and the "no … Ethereum clients" wording; the remaining evidence covers go/rust/js and Kubo only.
- `spec.md`: drop "Nu is not an Ethereum consensus node" from the non-goals (no need to mention Ethereum at all).
- Backlog files 0011 and 0002: reword the title/acceptance text so they no longer name Ethereum (record in the work log that the Ethereum criterion was withdrawn).

Keep: secp256k1 *verification* in `p2p/wire/keys.go` (libp2p peer-id spec key type) and secp256k1 signing/verification in `core/atrepo` (atproto accounts use k256 keys). The `github.com/decred/dcrd/dcrec/secp256k1/v4` dependency stays for those; update the D18 dependency comment in spec.md to say "libp2p and atproto secp256k1" if it mentions Ethereum. Nu's own node identity stays Ed25519.

Acceptance: `grep -riI 'ethereum\|lighthouse\|lodestar\|eth2' --exclude-dir=.git --exclude-dir=.ycc --exclude-dir=.worktrees .` returns nothing outside backlog work-log history; `python3 interop/run.py blackbox` (Kubo) and the transport matrix still run; `go build ./...` in the root and `interop/` modules passes.

## Acceptance criteria

## Work log
- 2026-10-22: Done (05eb761, 3e14086; merged d536e3d): eth2 ping + snappy dep removed from interop/, Lighthouse/Lodestar gone from run.py/README/CI job name, docs/spec Ethereum mentions removed, backlog 0011/0002 reworded with work-log notes. Kubo blackbox and the go/rust/js transport matrix (6/6) pass after removal. Remaining grep hits are only backlog history; memory notes consolidated.
