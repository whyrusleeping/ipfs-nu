#!/usr/bin/env bash
# 0029 automated exit. Run only on Linux with namespaces/nft/ip/tc and Docker.
# No public peers or fleet is contacted; all relays/peers are disposable fixtures.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ ${NU_SLOW:-} != 1 ]]; then
  echo 'Opt in explicitly: NU_SLOW=1 ./scripts/magic-preview.sh' >&2
  exit 2
fi
# Force the acceptance size: use go test directly for NU_MAGIC_BYTES smoke runs.
unset NU_MAGIC_BYTES
out=${NU_MAGIC_RESULTS:-interop/results/0029}
mkdir -p "$out"
# Guard both the workspace and test data filesystem before doing any big work.
# The Go test repeats df/statfs before each big transfer; do not bypass it.
for dir in . "${TMPDIR:-/tmp}"; do
  df -h "$dir"
  available=$(df -Pk "$dir" | awk 'NR==2 {print $4}')
  if (( available < 40*1024*1024 )); then
    echo "Abort: less than 40 GiB free on $dir" >&2
    exit 1
  fi
done
{
  date -u
  git rev-parse HEAD
  git status --short
  uname -a
  go version
  docker version --format '{{.Server.Version}}'
} | tee "$out/environment.log"
NU_SLOW=1 go test -tags natlab ./node/swarm -run '^TestMagicPreview$' \
  -count=1 -v -timeout=56m 2>&1 | tee "$out/natlab.log"
# Sequential: keep Docker builds out of the bulk-transfer disk/perf budget.
python3 interop/run.py transport --daemon --impl all --out "$out/transport"
python3 interop/run.py blackbox --daemon --client kubo --out "$out/kubo"
df -h . "${TMPDIR:-/tmp}"
# Go cleans every temporary source/store/output, and run.py removes its containers,
# volumes and private networks. Small result logs and reusable Docker build caches
# are retained; never prune unrelated resources on a shared machine.
