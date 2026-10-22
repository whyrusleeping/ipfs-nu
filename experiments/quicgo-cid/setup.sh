#!/bin/sh
# Recreates ./quic-go (gitignored): stock quic-go v0.64.1 from the Go module
# cache (checksum-verified by `go mod download`) plus the proposed upstream
# patch, committed in a throwaway git repo so `git -C quic-go log -p` shows it.
set -eu
cd "$(dirname "$0")"
version=v0.64.1
if [ -e quic-go ]; then
	echo "quic-go/ already exists; remove it to recreate" >&2
	exit 0
fi
dir=$(go mod download -json "github.com/quic-go/quic-go@$version" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$dir" ] || { echo "go mod download failed" >&2; exit 1; }
rm -rf quic-go.tmp
trap 'rm -rf quic-go.tmp' EXIT
cp -R "$dir" quic-go.tmp
chmod -R u+w quic-go.tmp
g() { git -C quic-go.tmp -c user.name=setup -c user.email=setup@localhost -c commit.gpgsign=false "$@"; }
g init -q
g add -A
g commit -q -m "quic-go $version (module zip)"
g am -q ../0001-add-Config.ConnectionIDTracer.patch
mv quic-go.tmp quic-go
echo "patched quic-go $version ready in $(pwd)/quic-go"
