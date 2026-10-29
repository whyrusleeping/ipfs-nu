#!/usr/bin/env bash
# Synthetic Nu transfer benchmark between real machines (docs/validation.md "Synthetic
# transfer benchmark"). bench/cmd/xferbench serves generated content from a tmpfs store on
# one or two source hosts and fetches it on a third, so neither disk is involved:
#
#   SRC1=alphac-2 SRC2=alphac-1 DST=alphac-3 REPEAT=3 ./scripts/xfer-bench.sh [workload...]
#
# Workloads (default "big tree"): big = 1 × 4 GiB, mid = 1,000 × 4 MiB, tree = 10,000 ×
# 100 KiB. MODES (default "discard fetcher"): discard verifies and drops every byte (the
# transfer ceiling); fetcher runs Nu's fetcher into a fresh tmpfs store (the receive
# pipeline without disk). SOURCES (default "1 2"): 1:1 from SRC1, 2:1 from SRC1 and SRC2,
# which serve identical content. Each fetch line is one run; "tx" is what each source's
# NIC sent across all runs of that line. Needs ssh to the hosts and RAM for the content
# on every host.
set -euo pipefail

SRC1=${SRC1:-alphac-2} SRC2=${SRC2:-alphac-1} DST=${DST:-alphac-3}
REPEAT=${REPEAT:-3} MODES=${MODES:-discard fetcher} SOURCES=${SOURCES:-1 2} PORT=${PORT:-4181}
OUT=${OUT:-/tmp/xfer-bench-$(date +%Y%m%d-%H%M%S).log}
workloads=${*:-big tree}

log() { echo "$*" | tee -a "$OUT"; }
r() { local h=$1; shift; ssh -o BatchMode=yes "$h" "$@"; }
ipof() { getent hosts "$1" | awk '{print $1}'; }
iface() { r "$1" "ip -o route get $(ipof "$DST") | sed -n 's/.* dev \([^ ]*\).*/\1/p'"; }
txbytes() { r "$1" "cat /sys/class/net/$2/statistics/tx_bytes"; }
flags() {
  case $1 in
    big) echo "-files 1 -size 4GiB" ;;
    mid) echo "-files 1000 -size 4MiB" ;;
    tree) echo "-files 10000 -size 100KiB" ;;
    *) echo "unknown workload $1" >&2; exit 2 ;;
  esac
}
sources() { if [ "$SOURCES" = 1 ]; then echo "$SRC1"; else echo "$SRC1 $SRC2"; fi; }

# startserve HOST WORKLOAD: start a server and print its link once it is serving.
startserve() {
  local f; f=$(flags "$2")
  r "$1" "mkdir -p nu-bench && cd nu-bench && { pkill -INT -x xferbench || true; } &&
    while pgrep -x xferbench >/dev/null; do sleep 0.2; done && rm -f xfer.link &&
    (setsid ./xferbench serve -listen /ip4/0.0.0.0/udp/$PORT/quic-v1 -addr /ip4/$(ipof "$1")/udp/$PORT/quic-v1 \
      $f -link-file xfer.link </dev/null >serve.log 2>&1 &) &&
    for i in \$(seq 1200); do [ -s xfer.link ] && break; pgrep -x xferbench >/dev/null || break; sleep 0.25; done
    [ -s xfer.link ] || { cat serve.log >&2; exit 1; }; cat xfer.link"
}
stopserve() {
  r "$1" "pkill -INT -x xferbench; while pgrep -x xferbench >/dev/null; do sleep 0.2; done; tail -1 nu-bench/serve.log"
}

CGO_ENABLED=0 go build -trimpath -o /tmp/xferbench-bin ./bench/cmd/xferbench
for h in $(sources) "$DST"; do r "$h" "mkdir -p nu-bench"; scp -q /tmp/xferbench-bin "$h:nu-bench/xferbench"; done
log "xfer-bench $(date -u +%FT%TZ) $(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty) SRC1=$SRC1 SRC2=$SRC2 DST=$DST REPEAT=$REPEAT workloads: $workloads modes: $MODES sources: $SOURCES"
for h in $(sources) "$DST"; do
  log "host $h: $(r "$h" "nproc") threads, $(r "$h" "ethtool -k $(iface "$h") 2>/dev/null | grep tx-udp-seg | tr -d '\t'"), load $(r "$h" "cut -d' ' -f1 /proc/loadavg")"
done

for w in $workloads; do
  declare -A link=()
  for h in $(sources); do link[$h]=$(startserve "$h" "$w"); done
  base=${link[$SRC1]}
  if [ "$SOURCES" != 1 ]; then
    [ "${base%%\?*}" = "${link[$SRC2]%%\?*}" ] || { log "CID mismatch for $w"; exit 1; }
  fi
  log "$w: $(r "$SRC1" "head -1 nu-bench/serve.log")"
  for n in $SOURCES; do
    l=$base hosts=$SRC1
    if [ "$n" = 2 ]; then l="$base&${link[$SRC2]#*\?}" hosts="$SRC1 $SRC2"; fi
    for m in $MODES; do
      declare -A tx0=() ifs=()
      for h in $hosts; do ifs[$h]=$(iface "$h"); tx0[$h]=$(txbytes "$h" "${ifs[$h]}"); done
      res=$(r "$DST" "cd nu-bench && ./xferbench fetch -mode $m -repeat $REPEAT '$l' 2>&1 | grep -v 'receive buffer' || true")
      split=""
      for h in $hosts; do split+=" $h=$(( ($(txbytes "$h" "${ifs[$h]}") - ${tx0[$h]}) / 1048576 ))MiB"; done
      while IFS= read -r line; do log "$w $n:1 $line"; done <<<"$res"
      log "$w $n:1 $m tx:$split"
    done
  done
  for h in $(sources); do log "$w server $h: $(stopserve "$h")"; done
done
log "log: $OUT"
