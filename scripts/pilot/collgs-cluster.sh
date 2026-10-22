#!/bin/sh
# Set up a CollGS pilot collection cluster (docs/collections.md "CollGS pilot runbook")
# on N hosts over SSH, in private mode WITHOUT multicast (swarm.mdns = false): members
# find each other only through the invite's boot addresses, member records and
# heartbeats.
#
#   collgs-cluster.sh [options] up    HOST[=ADDR]...   install, init, start, found, invite/join,
#                                                       writer keys, collection, push tokens
#   collgs-cluster.sh [options] smoke HOST[=ADDR]...   push items to the receiving members and
#                                                       wait until the index counts 2 copies of each
#   collgs-cluster.sh [options] status HOST[=ADDR]...  collection status on every host
#   collgs-cluster.sh [options] down  HOST[=ADDR]...   stop the daemons (state stays)
#
# HOST is an SSH destination ([user@]host). ADDR, if given, is the IP the host's swarm binds
# and advertises (default: all interfaces, advertised by interface expansion). The first
# HOST founds the cluster, holds the admin key and runs the holdings index; the first
# --writers HOSTs (default 2) are receiving members: each gets a writer key, a producer
# listener (api.producer, plain HTTP on a trusted network or VPN) and a push token.
#
# Options:
#   --nu PATH             nu binary to install (default: `go build ./cmd/nu` from this checkout)
#   --dir DIR             remote install dir (default .nu-pilot under the SSH user's home):
#                         DIR/bin/nu, NU_PATH=DIR/repo, daemon log DIR/daemon.log
#   --port N              swarm UDP port (default 4171; members must reach each other on it)
#   --cluster NAME        cluster name (default collgs)
#   --collection NAME     collection name (default s2)
#   --writers K           receiving members (default 2)
#   --producer-port N     producer listener port on receiving members (default 5172)
#   --smoke-items N       items the smoke test pushes (default 20)
#   --local DIR           validation mode: every HOST is a NU_PATH under DIR on this machine,
#                         on loopback with distinct ports (no SSH, nothing remote)
#
# The script never runs anything as root and installs no service; for a long-running
# pilot use dist/install.sh --user (systemd user unit) with the same configuration.
set -eu

nu_bin=
dir=.nu-pilot
port=4171
cluster=collgs
coll=s2
writers=2
pport=5172
smoke_items=20
local_dir=
here=$(cd "$(dirname "$0")/../.." && pwd)

usage() { sed -n '2,36p' "$0"; exit 2; }
while [ $# -gt 0 ]; do
	case "$1" in
	--nu) shift; nu_bin=$1 ;;
	--dir) shift; dir=$1 ;;
	--port) shift; port=$1 ;;
	--cluster) shift; cluster=$1 ;;
	--collection) shift; coll=$1 ;;
	--writers) shift; writers=$1 ;;
	--producer-port) shift; pport=$1 ;;
	--smoke-items) shift; smoke_items=$1 ;;
	--local) shift; local_dir=$1 ;;
	-h|--help) usage ;;
	-*) echo "collgs-cluster: unknown option $1" >&2; exit 2 ;;
	*) break ;;
	esac
	shift
done
[ $# -ge 2 ] || usage
cmd=$1
shift
[ $# -ge "$writers" ] || { echo "collgs-cluster: need at least $writers hosts (--writers)" >&2; exit 2; }

log() { echo "collgs-cluster: $*" >&2; }

# host_of / addr_of split HOST[=ADDR]; idx_of gives a host's 0-based position.
host_of() { echo "${1%%=*}"; }
addr_of() { case "$1" in *=*) echo "${1#*=}" ;; *) echo "" ;; esac; }
idx_of() {
	i=0
	for h in $all; do
		[ "$h" = "$1" ] && { echo $i; return; }
		i=$((i + 1))
	done
	echo "collgs-cluster: unknown host $1" >&2
	exit 1
}
all=$*

# Per-host ports: in --local mode every host gets its own ports on loopback.
swarm_port() { if [ -n "$local_dir" ]; then echo $((port + $(idx_of "$1"))); else echo "$port"; fi; }
api_port() { if [ -n "$local_dir" ]; then echo $((25171 + $(idx_of "$1"))); else echo 5171; fi; }
gw_port() { if [ -n "$local_dir" ]; then echo $((28171 + $(idx_of "$1"))); else echo 8171; fi; }
prod_port() { if [ -n "$local_dir" ]; then echo $((pport + 100 + $(idx_of "$1"))); else echo "$pport"; fi; }

# rsh HOSTSPEC SCRIPT runs a shell script on the host (sent on stdin, so it needs no
# quoting for SSH) in the install dir, with NU_PATH set and `nu` on PATH.
rsh() {
	h=$(host_of "$1")
	if [ -n "$local_dir" ]; then
		printf '%s\n' "$2" | (cd "$local_dir/$h" && NU_PATH="$PWD/repo" PATH="$PWD/bin:$PATH" sh -s)
	else
		printf '%s\n' "$2" | ssh -o BatchMode=yes "$h" "cd $dir && NU_PATH=\$PWD/repo PATH=\$PWD/bin:\$PATH sh -s"
	fi
}

# json_field KEY reads the first "KEY": "value" (or number) from JSON on stdin.
json_field() { tr -d '\n' | sed -n "s/.*\"$1\": *\"\{0,1\}\([^\",}]*\).*/\1/p"; }

install() {
	spec=$1
	h=$(host_of "$spec")
	if [ -n "$local_dir" ]; then
		mkdir -p "$local_dir/$h/bin"
		cp "$nu_bin" "$local_dir/$h/bin/nu"
	else
		ssh -o BatchMode=yes "$h" "mkdir -p $dir/bin"
		scp -q "$nu_bin" "$h:$dir/bin/nu.new"
		ssh -o BatchMode=yes "$h" "mv $dir/bin/nu.new $dir/bin/nu && chmod 755 $dir/bin/nu"
	fi
}

configure() {
	spec=$1
	i=$(idx_of "$spec")
	a=$(addr_of "$spec")
	[ -n "$local_dir" ] && a=127.0.0.1
	listen="[\"/ip4/0.0.0.0/udp/$(swarm_port "$spec")/quic-v1\"]"
	[ -n "$a" ] && listen="[\"/ip4/$a/udp/$(swarm_port "$spec")/quic-v1\"]"
	rsh "$spec" "nu init >/dev/null 2>&1 || true
		nu config set swarm.listen '$listen' >/dev/null &&
		nu config set swarm.mdns false >/dev/null &&
		nu config set api.listen 127.0.0.1:$(api_port "$spec") >/dev/null &&
		nu config set gateway.listen 127.0.0.1:$(gw_port "$spec") >/dev/null"
	if [ "$i" = 0 ]; then
		rsh "$spec" "nu config set cluster.index true >/dev/null"
	fi
	if [ "$i" -lt "$writers" ]; then
		phost=$a
		[ -n "$phost" ] || phost=$(host_of "$spec" | sed 's/.*@//')
		rsh "$spec" "nu config set api.producer.listen ${a:-0.0.0.0}:$(prod_port "$spec") >/dev/null &&
			nu config set api.producer.hosts '[\"$phost\", \"$(host_of "$spec" | sed 's/.*@//')\"]' >/dev/null &&
			nu config set api.producer.trusted_network true >/dev/null"
	fi
}

start() {
	# Other nu commands lock a repo without a daemon, so none runs while it starts: the
	# daemon is up once it has written its API address file.
	rsh "$1" "if nu status 2>/dev/null | grep -q 'daemon: *running'; then exit 0; fi
		rm -f \$NU_PATH/api
		setsid nohup nu daemon >>daemon.log 2>&1 </dev/null &
		for i in \$(seq 60); do
			if [ -s \$NU_PATH/api ] && nu status 2>/dev/null | grep -q 'daemon: *running'; then exit 0; fi
			sleep 0.5
		done
		echo daemon did not start; tail -20 daemon.log; exit 1"
}

stop() {
	rsh "$1" "p=\$(nu --json status 2>/dev/null | tr -d '\n' | sed -n 's/.*\"pid\": *\([0-9]*\).*/\1/p');
		[ -n \"\$p\" ] && kill \$p; true"
}

peer_id() { rsh "$1" "nu --json status" | json_field peer_id; }

up() {
	if [ -z "$nu_bin" ]; then
		nu_bin=$(mktemp -d)/nu
		log "building nu from $here"
		(cd "$here" && go build -o "$nu_bin" ./cmd/nu)
	fi
	first=$1
	for spec in $all; do
		log "$(host_of "$spec"): install, configure, start"
		install "$spec"
		configure "$spec"
		start "$spec"
	done
	if rsh "$first" "nu cluster status" 2>/dev/null | grep -q "network cluster"; then
		log "$(host_of "$first"): cluster exists, not founding again"
	else
		log "$(host_of "$first"): founding private cluster $cluster"
		rsh "$first" "nu cluster init $cluster --mode private" >/dev/null
	fi
	for spec in $all; do
		[ "$spec" = "$first" ] && continue
		if rsh "$spec" "nu cluster status" 2>/dev/null | grep -q "network cluster"; then
			continue
		fi
		p=$(peer_id "$spec")
		log "$(host_of "$spec"): joining as $p"
		tok=$(rsh "$first" "nu cluster invite --peer $p 2>/dev/null" | grep -o 'nuc1[a-z0-9]*' | head -1)
		rsh "$spec" "nu cluster join $tok" >/dev/null
	done
	# Writer keys on the receiving members, granted by the admin.
	wflags=
	i=0
	for spec in $all; do
		[ $i -lt "$writers" ] || break
		did=$(rsh "$spec" "cat writer.did 2>/dev/null || (nu --json cluster key new | tr -d '\n' | sed -n 's/.*\"did\": *\"\([^\"]*\)\".*/\1/p' | tee writer.did)")
		log "$(host_of "$spec"): writer key $did"
		wflags="$wflags --writer $did"
		dids="${dids:-} $did"
		i=$((i + 1))
	done
	if ! rsh "$first" "nu cluster dataset ls" 2>/dev/null | grep -q "^$coll\b"; then
		log "creating collection $coll (replicas 0, union writers)"
		rsh "$first" "cat > facets.toml <<'EOF'
sensed   = { ptr = \"/stac/properties/datetime\",       type = \"time\" }
bbox     = { ptr = \"/stac/bbox\",                      type = \"bbox\" }
cloud    = { ptr = \"/stac/properties/eo:cloud_cover\", type = \"number\" }
platform = { ptr = \"/stac/properties/platform\",       type = \"string\" }
EOF
nu cluster dataset create $coll --collection --facets facets.toml $wflags" >/dev/null
	fi
	for did in $dids; do
		rsh "$first" "nu cluster key grant $did writer --dataset $coll" >/dev/null
	done
	i=0
	for spec in $all; do
		[ $i -lt "$writers" ] || break
		# The index member and the receiving members restart to pick up config set
		# after their daemon started (cluster.index, api.producer).
		tok=$(rsh "$spec" "cat producer.token 2>/dev/null || (nu api token create producer --collection $coll | grep -o 'nu_pt_[A-Za-z0-9_-]*' | tee producer.token)")
		a=$(addr_of "$spec")
		[ -n "$local_dir" ] && a=127.0.0.1
		echo "producer: nu --api ${a:-$(host_of "$spec" | sed 's/.*@//')}:$(prod_port "$spec") --token $tok cluster collection put $coll <path> --meta meta.json --upload <files>"
		i=$((i + 1))
	done
	log "waiting for every member to see collection $coll"
	for spec in $all; do
		n=0
		until rsh "$spec" "nu cluster collection status $coll" >/dev/null 2>&1; do
			n=$((n + 1))
			[ $n -lt 120 ] || { log "$(host_of "$spec") never saw $coll"; exit 1; }
			sleep 1
		done
	done
	log "up: $(echo "$all" | wc -w) members, index on $(host_of "$first"); dashboard shim:"
	echo "  ssh -L 8080:127.0.0.1:8080 $(host_of "$first")   # then on it:"
	echo "  go run ./scripts/pilot/collgs-dash -api 127.0.0.1:$(api_port "$first") -collection $coll"
}

# smoke pushes items to every receiving member (the same items, as a producer with two
# receiving members does) and waits for the index to count two copies of each.
smoke() {
	first=$1
	ts=$(date -u +%Y%m%dT%H%M%S)
	k=0
	while [ $k -lt "$smoke_items" ]; do
		id="SMOKE_${ts}_$k"
		i=0
		for spec in $all; do
			[ $i -lt "$writers" ] || break
			rsh "$spec" "mkdir -p smoke && printf '%s' 'cog stand-in $id' > smoke/$id.tif &&
				printf '%s' '{\"stac\": {\"id\": \"$id\", \"bbox\": [6, 46, 7, 47], \"properties\": {\"datetime\": \"2026-10-21T10:00:00Z\", \"eo:cloud_cover\": $k, \"platform\": \"sentinel-2b\"}}}' > smoke/$id.json &&
				nu cluster collection put $coll smoke/$ts/$id --meta smoke/$id.json $id.tif=\$PWD/smoke/$id.tif" >/dev/null
			i=$((i + 1))
		done
		k=$((k + 1))
	done
	if [ -n "$local_dir" ]; then
		# The producer listener with its token, from "another machine".
		spec=$(echo "$all" | cut -d' ' -f1)
		tok=$(cat "$local_dir/$(host_of "$spec")/producer.token")
		echo '{"stac": {"id": "SMOKE_PRODUCER"}}' > "$local_dir/producer-meta.json"
		echo "via the producer listener" > "$local_dir/producer.tif"
		NU_PATH="$local_dir/producer-client" "$local_dir/$(host_of "$spec")/bin/nu" --api 127.0.0.1:$(prod_port "$spec") --token "$tok" \
			cluster collection put "$coll" "smoke/$ts/SMOKE_PRODUCER" --meta "$local_dir/producer-meta.json" "visual.tif=$local_dir/producer.tif" --wait >/dev/null
		log "pushed one item through the producer listener with a push token"
	fi
	want=$((smoke_items * writers))
	log "pushed $smoke_items items to $writers receiving members; waiting for the index on $(host_of "$first")"
	n=0
	while :; do
		got=$(rsh "$first" "nu --json cluster collection items $coll --prefix smoke/$ts --holders --limit 1000" |
			tr ',' '\n' | grep -c '"state": *"complete"' || true)
		[ "$got" -ge "$want" ] && break
		n=$((n + 1))
		[ $n -lt 180 ] || { log "timed out: the index counts $got of $want complete holdings"; exit 1; }
		sleep 1
	done
	log "smoke ok: the index counts $got complete holdings of $smoke_items items ($writers copies each) after ${n}s"
}

status_all() {
	for spec in $all; do
		echo "== $(host_of "$spec")"
		rsh "$spec" "nu cluster collection status $coll" || true
	done
}

case "$cmd" in
up) up "$@" ;;
smoke) smoke "$@" ;;
status) status_all ;;
down) for spec in $all; do stop "$spec"; done ;;
*) usage ;;
esac
