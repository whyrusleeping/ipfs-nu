#!/bin/sh
# testtimes.sh: run `go test -json ./...` and report per-package and per-test wall time.
#
# Usage: scripts/testtimes.sh [-n TOP] [-- extra go test args]
#   TOP  number of slowest tests/packages to print (default 25)
#
# The raw JSON stream is kept in $NU_TESTTIMES_JSON (default: a temp file, path printed) so
# it can be re-reported with: scripts/testtimes.sh -r FILE
set -eu

top=25
report=""
while [ $# -gt 0 ]; do
	case "$1" in
	-n) top=$2; shift 2 ;;
	-r) report=$2; shift 2 ;;
	--) shift; break ;;
	*) break ;;
	esac
done

if [ -z "$report" ]; then
	report=${NU_TESTTIMES_JSON:-$(mktemp -t nu-testtimes.XXXXXX.json)}
	echo "load: $(cut -d' ' -f1-3 /proc/loadavg)  cores: $(nproc)  json: $report" >&2
	start=$(date +%s.%N)
	# -json output only; the exit status is reported after the table.
	go test -json -count=1 "$@" ./... >"$report" 2>&1 || true
	end=$(date +%s.%N)
	echo "wall: $(echo "$end - $start" | bc) s  load after: $(cut -d' ' -f1-3 /proc/loadavg)" >&2
fi

# Per-test rows come from "pass"/"fail"/"skip" actions with a Test field; per-package rows
# from the same actions without one. Subtests are reported alongside top-level tests.
awk -v top="$top" '
/^\{/ {
	action=""; pkg=""; test=""; elapsed=""
	if (match($0, /"Action":"[a-z]+"/)) action=substr($0, RSTART+10, RLENGTH-11)
	if (match($0, /"Package":"[^"]+"/)) pkg=substr($0, RSTART+11, RLENGTH-12)
	if (match($0, /"Test":"[^"]+"/)) test=substr($0, RSTART+8, RLENGTH-9)
	if (match($0, /"Elapsed":[0-9.e+-]+/)) elapsed=substr($0, RSTART+10, RLENGTH-10)
	if (action != "pass" && action != "fail" && action != "skip") next
	if (test == "") {
		pkgs[pkg]=elapsed+0; pkgres[pkg]=action
		if (action == "fail") failed=failed " " pkg
		if (elapsed+0 > maxpkg) maxpkg=elapsed+0
	} else {
		key=pkg " " test
		tests[key]=elapsed+0; testres[key]=action
		if (action == "fail") failedtests=failedtests "\n  " key
	}
}
END {
	n=0
	print "slowest packages:"
	cmd="sort -rn | head -n " top
	for (p in pkgs) printf "%8.2fs %-5s %s\n", pkgs[p], pkgres[p], p | cmd
	close(cmd)
	print ""
	print "slowest tests:"
	cmd="sort -rn | head -n " top
	for (k in tests) printf "%8.2fs %-5s %s\n", tests[k], testres[k], k | cmd
	close(cmd)
	print ""
	printf "longest package: %.2fs\n", maxpkg
	if (failed != "") print "FAILED packages:" failed
	if (failedtests != "") print "FAILED tests:" failedtests
	if (failed == "") print "all packages passed"
}' "$report"
