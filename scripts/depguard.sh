#!/bin/sh
# Fails if a forbidden dependency appears in the main module's go.mod or build graph.
# go-libp2p and tailscale.com are never allowed (spec D18; CONTRIBUTING.md). Spikes that
# need them live in the separate experiments/ module.
set -eu
cd "$(dirname "$0")/.."
re='(github\.com/libp2p/go-libp2p|tailscale\.com)([/ ]|$)'
if grep -En "$re" go.mod >&2; then
	echo "forbidden dependency in go.mod (see CONTRIBUTING.md)" >&2
	exit 1
fi
mods=$(go list -m all)
if echo "$mods" | awk '{print $1" "}' | grep -E "^$re" >&2; then
	echo "forbidden dependency in the module graph (see CONTRIBUTING.md)" >&2
	exit 1
fi
