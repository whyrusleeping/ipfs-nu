#!/usr/bin/env bash
# LAN benchmark of nu add and nu get between real machines (docs/validation.md "LAN
# benchmarks"). Run from a machine with ssh access (passwordless sudo for drop_caches)
# to three hosts:
#
#   SRC1=alphac-2 SRC2=alphac-1 DST=alphac-3 RUNS=3 ./scripts/lan-bench.sh [phase...]
#
# Phases (default all): deploy gen baseline add get1 get2 clean; also xfer1 xfer2.
# Workloads (WORKLOADS, default "big tree"): big = one 10 GiB file; tree = 10,000 files of
# 100 KiB (~1 GiB); mid = 1,000 files of 4 MiB (4 GiB); group = 20 files of 256 MiB (5 GiB).
# Both sources generate identical bytes (seeded), so they add to the same CIDs; gen skips
# workloads already generated. Every timed run starts from a fresh repo with page caches
# dropped on all hosts. The recipient is daemonless (`nu get <link> -o out`, the J1
# journey); sources run `nu daemon`. PROF=1 builds with -tags nuprof and saves CPU
# profiles of each recipient run and of the source daemons under $OUT.prof/; PROF=2 adds
# block profiles (waits on locks, channels, the database), which slow I/O loops. xfer1/xfer2
# are get1/get2 with bench/cmd/xferbench as the recipient: the same requests, verified and
# discarded, nothing stored (the transfer ceiling without the recipient's disk).
set -euo pipefail

SRC1=${SRC1:-alphac-2} SRC2=${SRC2:-alphac-1} DST=${DST:-alphac-3}
RUNS=${RUNS:-3} BIG_GIB=${BIG_GIB:-10} TREE_FILES=${TREE_FILES:-10000} TREE_KIB=${TREE_KIB:-100}
WORKLOADS=${WORKLOADS:-big tree} PROF=${PROF:-}
BLOCK=$( [ "$PROF" = 2 ] && echo 1 || true)
D='$HOME/nu-bench'           # remote work dir (expanded remotely)
OUT=${OUT:-/tmp/lan-bench-$(date +%Y%m%d-%H%M%S).log}
phases=${*:-deploy gen baseline add get1 get2 clean}

log() { echo "$*" | tee -a "$OUT"; }
r() { local h=$1; shift; ssh -o BatchMode=yes "$h" "$@"; }
drop() { local x; for x in "$SRC1" "$SRC2" "$DST"; do r "$x" 'sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null'; done; }
iface() { r "$1" "ip -o route get $(getent hosts "$DST" | awk '{print $1}') | sed -n 's/.* dev \([^ ]*\).*/\1/p'"; }
txbytes() { r "$1" "cat /sys/class/net/$2/statistics/tx_bytes"; }

deploy() {
  CGO_ENABLED=0 go build -trimpath ${PROF:+-tags nuprof} -o /tmp/nu-bench-bin ./cmd/nu
  CGO_ENABLED=0 go build -trimpath -o /tmp/xferbench-bin ./bench/cmd/xferbench
  for h in "$SRC1" "$SRC2" "$DST"; do
    r "$h" "mkdir -p $D && pkill -x nu || true"
    scp -q /tmp/nu-bench-bin "$h:nu-bench/nu"
  done
  scp -q /tmp/xferbench-bin "$DST:nu-bench/xferbench"
  log "deploy: $(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty)${PROF:+ nuprof} to $SRC1 $SRC2 $DST"
}

# wbytes WORKLOAD: total bytes; wpath WORKLOAD: path under data/ ("-r" for directories).
wbytes() { case $1 in big) echo $((BIG_GIB<<30));; tree) echo $((TREE_FILES*TREE_KIB*1024));; mid) echo $((1000<<22));; group) echo $((20<<28));; esac; }
wadd() { case $1 in big) echo data/big.bin;; *) echo "-r data/$1";; esac; }
# files N MIB DIR: N seeded pseudo-random files of MIB MiB in DIR (identical on every host).
files() { echo "mkdir -p data/$3 && for i in \$(seq -w 1 $1); do openssl enc -aes-128-ctr -nosalt -pass pass:nu-bench-$3-\$i -pbkdf2 </dev/zero 2>/dev/null | head -c \$(($2<<20)) > data/$3/f\$i.bin; done"; }

gen() {
  for h in "$SRC1" "$SRC2"; do
    local w cmds=""
    for w in $WORKLOADS; do
      case $w in
        big) cmds+="[ -f data/big.bin ] || openssl enc -aes-128-ctr -nosalt -pass pass:nu-bench -pbkdf2 </dev/zero 2>/dev/null | head -c $((BIG_GIB<<30)) > data/big.bin; " ;;
        tree) cmds+="[ -d data/tree ] || python3 -c '
import random,os
r=random.Random(1)
for i in range($TREE_FILES):
    d=\"data/tree/d%03d\"%(i//100); os.makedirs(d,exist_ok=True)
    open(\"%s/f%05d.bin\"%(d,i),\"wb\").write(r.randbytes($TREE_KIB*1024))
'; " ;;
        mid) cmds+="[ -d data/mid ] || { $(files 1000 4 mid); }; " ;;
        group) cmds+="[ -d data/group ] || { $(files 20 256 group); }; " ;;
      esac
    done
    r "$h" "cd $D && mkdir -p data && $cmds du -sb data/* | tr '\n' ' '" &
  done
  wait
  log "gen: $WORKLOADS on $SRC1 $SRC2"
}

baseline() {
  # TCP ceiling (one stream, nc) and recipient disk write ceiling.
  r "$DST" "pkill -x nc || true; (nc -l 5999 </dev/null >/dev/null 2>&1 &) ; sleep 0.5"
  local ip; ip=$(getent hosts "$DST" | awk '{print $1}')
  local t; t=$(r "$SRC1" "s=\$(date +%s.%N); dd if=/dev/zero bs=1M count=10240 status=none | nc -N $ip 5999; echo \"\$(date +%s.%N) - \$s\" | bc")
  log "baseline: tcp nc 10 GiB $SRC1->$DST: $(echo "10737.418 / $t" | bc) MB/s ($t s)"
  t=$(r "$DST" "cd $D && s=\$(date +%s.%N); dd if=/dev/zero of=ddtest bs=1M count=10240 conv=fdatasync status=none; e=\$(date +%s.%N); rm ddtest; echo \"\$e - \$s\" | bc")
  log "baseline: $DST disk write 10 GiB fdatasync: $(echo "10737.418 / $t" | bc) MB/s"
  t=$(r "$SRC1" "cd $D && sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null; s=\$(date +%s.%N); cat data/big.bin >/dev/null; echo \"\$(date +%s.%N) - \$s\" | bc")
  log "baseline: $SRC1 cold read big.bin: $(echo "$((BIG_GIB<<30)) / 1000000 / $t" | bc) MB/s"
}

# addrun HOST WORKLOAD [flags]: fresh repo, cold cache, daemonless add; prints CID.
addrun() {
  local h=$1 w=$2; shift 2
  r "$h" "cd $D && rm -rf repo && NU_PATH=$D/repo ./nu init >/dev/null 2>&1"
  drop
  r "$h" "cd $D && /usr/bin/time -f 'TIME %e s user %U sys %S maxrss %M KB' env NU_PATH=$D/repo ./nu --json add $* $(wadd "$w") 2>err.txt >add.json; grep TIME err.txt; grep -o '\"cid\": *\"[^\"]*\"' add.json | head -1 | cut -d'\"' -f4; du -sb repo | cut -f1"
}

add() {
  for w in $WORKLOADS; do
    local bytes; bytes=$(wbytes $w)
    for flags in "" "--nocopy"; do
      for i in $(seq "$RUNS"); do
        mapfile -t res < <(addrun "$SRC1" $w $flags)
        local e; e=$(echo "${res[0]}" | awk '{print $2}')
        log "add $w ${flags:-copy} run $i: ${res[0]} | $(echo "$bytes / 1000000 / $e" | bc) MB/s | repo $(( ${res[2]} / 1048576 )) MiB | ${res[1]}"
      done
    done
  done
}

# prepare HOST...: fresh repo with the workloads added (copy) and a daemon running.
prepare() {
  for h in "$@"; do
    local w adds=""
    for w in $WORKLOADS; do adds+="./nu --json add $(wadd $w) >$w.json 2>/dev/null && "; done
    r "$h" "cd $D && pkill -x nu; sleep 0.5; rm -rf repo && export NU_PATH=$D/repo && ./nu init >/dev/null 2>&1 && $adds
      (${PROF:+NU_CPUPROFILE=$D/daemon.cpu} ${BLOCK:+NU_BLOCKPROFILE=$D/daemon.block} setsid ./nu daemon </dev/null >daemon.log 2>&1 &) && sleep 2 && ./nu status | grep -c 'peer id'" >/dev/null &
  done
  wait
}
# srcprof HOST...: stop the source daemons, flushing and fetching their CPU profiles.
srcprof() {
  [ -n "$PROF" ] || return 0
  local h; mkdir -p "$OUT.prof"
  for h in "$@"; do
    r "$h" "pkill -INT -x nu; while pgrep -x nu >/dev/null; do sleep 0.2; done"
    local t; t=$(date +%H%M%S)
    scp -q "$h:nu-bench/daemon.cpu" "$OUT.prof/src-$h-$t.cpu" 2>/dev/null || true
    [ -z "$BLOCK" ] || scp -q "$h:nu-bench/daemon.block" "$OUT.prof/src-$h-$t.block" 2>/dev/null || true
  done
}
cidof() { r "$1" "grep -o '\"cid\": *\"[^\"]*\"' $D/$2.json | head -1 | cut -d'\"' -f4"; }
peerof() { r "$1" "NU_PATH=$D/repo $D/nu --json status" | grep -o '"peer_id": *"[^"]*"' | head -1 | cut -d'"' -f4; }
addrof() { echo "/ip4/$(getent hosts "$1" | awk '{print $1}')/udp/4171/quic-v1"; }

# getrun LABEL WORKLOAD LINK SRCHOSTS...: fresh daemonless recipient, cold caches.
getrun() {
  local label=$1 w=$2 link=$3; shift 3
  local bytes; bytes=$(wbytes $w)
  r "$DST" "cd $D && rm -rf rrepo out get.cpu get.block"
  drop
  declare -A tx0; local h ifs=()
  for h in "$@"; do ifs+=("$(iface "$h")"); tx0[$h]=$(txbytes "$h" "${ifs[-1]}"); done
  local res; res=$(r "$DST" "cd $D && s=\$(date +%s.%N); ${PROF:+NU_CPUPROFILE=$D/get.cpu} ${BLOCK:+NU_BLOCKPROFILE=$D/get.block} NU_PATH=$D/rrepo ./nu get '$link' -o out 2>&1 | while read -r l; do echo \"\$(echo \"\$(date +%s.%N) - \$s\" | bc) \$l\"; done; echo \"END \$(echo \"\$(date +%s.%N) - \$s\" | bc)\"")
  local total fetch_start write_start
  total=$(echo "$res" | awk '/^END/{print $2}')
  fetch_start=$(echo "$res" | awk '$2=="fetching"{print $1; exit}')
  write_start=$(echo "$res" | awk '$2=="writing"{print $1; exit}')
  local fetch=${write_start:-$total}; fetch=$(echo "$fetch - ${fetch_start:-0}" | bc)
  local split="" i=0
  for h in "$@"; do split+=" $h=$(( ($(txbytes "$h" "${ifs[$i]}") - ${tx0[$h]}) / 1048576 ))MiB"; i=$((i+1)); done
  local ok; ok=$(echo "$res" | grep -c '^[0-9.]* wrote' || true)
  log "get $label $w: total ${total}s, fetch ${fetch}s = $(echo "$bytes / 1000000 / $fetch" | bc) MB/s, write-out $(echo "$total - ${write_start:-$total}" | bc)s | tx:$split | ok=$ok"
  [ "$ok" = 1 ] || log "$res"
  if [ -n "$PROF" ]; then mkdir -p "$OUT.prof"; scp -q "$DST:nu-bench/get.cpu" "$OUT.prof/get-${label// /_}-$w.cpu"; [ -z "$BLOCK" ] || scp -q "$DST:nu-bench/get.block" "$OUT.prof/get-${label// /_}-$w.block"; fi
}

# xferrun LABEL WORKLOAD LINK SRCHOSTS...: transfer only (xferbench), cold caches.
xferrun() {
  local label=$1 w=$2 link=$3; shift 3
  drop
  declare -A tx0; local h ifs=()
  for h in "$@"; do ifs+=("$(iface "$h")"); tx0[$h]=$(txbytes "$h" "${ifs[-1]}"); done
  local res; res=$(r "$DST" "cd $D && ./xferbench ${PROF:+-cpuprofile $D/xfer.cpu} '$link' 2>&1")
  local split="" i=0
  for h in "$@"; do split+=" $h=$(( ($(txbytes "$h" "${ifs[$i]}") - ${tx0[$h]}) / 1048576 ))MiB"; i=$((i+1)); done
  log "xfer $label $w: ${res#xfer: } | tx:$split"
  if [ -n "$PROF" ]; then mkdir -p "$OUT.prof"; scp -q "$DST:nu-bench/xfer.cpu" "$OUT.prof/xfer-${label// /_}-$w.cpu"; fi
}

verify() { # compare the last recipient output with the source
  local w=$1
  if [ "$w" = big ]; then
    log "verify big: src $(r "$SRC1" "sha256sum $D/data/big.bin | cut -c1-16") dst $(r "$DST" "sha256sum $D/out | cut -c1-16")"
  else
    log "verify $w: src $(r "$SRC1" "cd $D/data/$w && find . -type f | sort | xargs sha256sum | sha256sum | cut -c1-16") dst $(r "$DST" "cd $D/out && find . -type f | sort | xargs sha256sum | sha256sum | cut -c1-16")"
  fi
}

get1() {
  r "$SRC2" "pkill -x nu || true"
  prepare "$SRC1"
  local p1; p1=$(peerof "$SRC1")
  for w in $WORKLOADS; do
    local c; c=$(cidof "$SRC1" $w)
    for i in $(seq "$RUNS"); do getrun "1:1 run $i" $w "nu://$c?peer=$p1&addr=$(addrof "$SRC1")" "$SRC1"; done
    verify $w
  done
  srcprof "$SRC1"
}

get2() {
  prepare "$SRC1" "$SRC2"
  local p1 p2; p1=$(peerof "$SRC1"); p2=$(peerof "$SRC2")
  for w in $WORKLOADS; do
    local c1 c2; c1=$(cidof "$SRC1" $w); c2=$(cidof "$SRC2" $w)
    [ "$c1" = "$c2" ] || { log "CID mismatch $w: $c1 vs $c2"; exit 1; }
    for i in $(seq "$RUNS"); do
      getrun "2:1 run $i" $w "nu://$c1?peer=$p1&addr=$(addrof "$SRC1")&peer=$p2&addr=$(addrof "$SRC2")" "$SRC1" "$SRC2"
    done
    verify $w
  done
  srcprof "$SRC1" "$SRC2"
}

xfer1() {
  r "$SRC2" "pkill -x nu || true"
  prepare "$SRC1"
  local p1 w c i; p1=$(peerof "$SRC1")
  for w in $WORKLOADS; do
    c=$(cidof "$SRC1" $w)
    for i in $(seq "$RUNS"); do xferrun "1:1 run $i" $w "nu://$c?peer=$p1&addr=$(addrof "$SRC1")" "$SRC1"; done
  done
  srcprof "$SRC1"
}

xfer2() {
  prepare "$SRC1" "$SRC2"
  local p1 p2 w c i; p1=$(peerof "$SRC1"); p2=$(peerof "$SRC2")
  for w in $WORKLOADS; do
    c=$(cidof "$SRC1" $w)
    for i in $(seq "$RUNS"); do
      xferrun "2:1 run $i" $w "nu://$c?peer=$p1&addr=$(addrof "$SRC1")&peer=$p2&addr=$(addrof "$SRC2")" "$SRC1" "$SRC2"
    done
  done
  srcprof "$SRC1" "$SRC2"
}

clean() { for h in "$SRC1" "$SRC2" "$DST"; do r "$h" "pkill -x nu || true; rm -rf $D/repo $D/rrepo $D/out"; done; log "clean: repos removed (data kept in ~/nu-bench/data)"; }

log "lan-bench $(date -u +%FT%TZ) SRC1=$SRC1 SRC2=$SRC2 DST=$DST RUNS=$RUNS workloads: $WORKLOADS phases: $phases"
for p in $phases; do $p; done
log "log: $OUT"
