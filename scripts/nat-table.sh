#!/bin/sh
# Per-pair NAT success table from the software NAT lab (docs/validation.md "Per-pair NAT
# success table"; release gate, see .github/workflows/release.yml).
#
#   scripts/nat-table.sh <outdir>
#
# Runs p2p/disco's pair matrix (TestHardNATLab) plus the public/IPv6/router-mapped rows in
# isolated network namespaces, then writes <outdir>/results.ndjson, <outdir>/natlab.log and
# <outdir>/nat-pairs.md. Exits non-zero when any test or gated row fails or a row is missing.
# Env: CAMPAIGN (trials for EIM↔random-APDM rows, default 50), TRIALS (other rows, default 3),
# NU_MINIUPNPD (miniupnpd binary; required, else looked up on PATH), NAT_SUDO=1 to run the
# compiled test binary with sudo (CI runners without rootless user namespaces). Linux only;
# needs ip, nft, unshare and nsenter.
set -eu

out=${1:-}
[ -n "$out" ] || { sed -n '2,13p' "$0"; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
campaign=${CAMPAIGN:-50}
miniupnpd=${NU_MINIUPNPD:-$(command -v miniupnpd || true)}
[ -n "$miniupnpd" ] || { echo "nat-table.sh: miniupnpd not found (set NU_MINIUPNPD); the mapped row is required" >&2; exit 2; }

cd "$root"
go build -o "$out/nattable" ./p2p/natlab/cmd/nattable
go test -c -tags natlab -o "$out/disco.test" ./p2p/disco
rm -f "$out/results.ndjson"
sudo=
[ "${NAT_SUDO:-}" = 1 ] && sudo=sudo
status=0
$sudo env NU_NATLAB_RESULTS="$out/results.ndjson" NU_MINIUPNPD="$miniupnpd" \
	NU_HARD_TRIALS="${TRIALS:-3}" NU_HARD_RANDOM_TRIALS="$campaign" \
	"$out/disco.test" -test.v -test.count=1 -test.timeout=70m \
	-test.run '^(TestHardNATLab|TestNATLab(PublicEIM|IPv6|MappedMiniUPnPd))$' >"$out/natlab.log" 2>&1 || status=$?
[ -z "$sudo" ] || sudo chown "$(id -u):$(id -g)" "$out/results.ndjson" 2>/dev/null || true
{
	echo "Nu per-pair NAT success table (software NAT lab: Linux namespaces + nftables, not consumer routers)"
	echo
	echo "- revision: $(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)$(git diff --quiet HEAD 2>/dev/null || echo ' (dirty)')"
	echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ); kernel: $(uname -r); EIM↔random campaign: $campaign trials"
	echo
	"$out/nattable" -lab -campaign "$campaign" "$out/results.ndjson"
} >"$out/nat-pairs.md" || status=1
cat "$out/nat-pairs.md"
[ "$status" = 0 ] || echo "nat-table.sh: FAILED (see $out/natlab.log)" >&2
exit "$status"
