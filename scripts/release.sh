#!/bin/sh
# Build Nu release artifacts (docs/operations.md "Distribution").
#
#   scripts/release.sh build   <version> <outdir>   static binaries for every target
#   scripts/release.sh package <version> <outdir>   tarballs + SHA256SUMS from built binaries
#
# build writes <outdir>/bin/<os>_<arch>/nu for linux/amd64, linux/arm64, darwin/amd64 and
# darwin/arm64 (CGO_ENABLED=0, -trimpath, version stamped into nu.Version; Linux binaries
# are checked to be statically linked). Signing a macOS binary (or a darwin_universal one
# made with lipo) replaces it in place before package runs. package writes
# <outdir>/nu_<version>_<os>_<arch>.tar.gz for every <outdir>/bin/<os>_<arch>/nu, each
# holding the binary, LICENSE, NOTICE, dist/install.sh and the service definitions, with
# fixed owner/order/mtime so the same binaries give the same tarballs, then SHA256SUMS.
set -eu

cmd=${1:-}
version=${2:-}
out=${3:-}
[ -n "$cmd" ] && [ -n "$version" ] && [ -n "$out" ] || { sed -n '2,13p' "$0"; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)

case "$cmd" in
build)
	for target in ${TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}; do
		goos=${target%/*} goarch=${target#*/}
		dir=$out/bin/${goos}_$goarch
		mkdir -p "$dir"
		(cd "$root" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -buildvcs=false \
			-ldflags "-s -w -X github.com/whyrusleeping/nu.Version=$version" -o "$dir/nu" ./cmd/nu)
		if [ "$goos" = linux ] && command -v file >/dev/null 2>&1; then
			file "$dir/nu" | grep -q 'statically linked' || { echo "release.sh: $dir/nu is not static" >&2; exit 1; }
		fi
		echo "built $dir/nu"
	done
	;;
package)
	cd "$out"
	rm -f SHA256SUMS
	for dir in bin/*_*; do
		[ -f "$dir/nu" ] || continue
		plat=${dir#bin/}
		name=nu_${version}_$plat
		stage=$out/stage/$name
		rm -rf "$stage"
		mkdir -p "$stage"
		cp "$dir/nu" "$stage/nu"
		cp "$root/LICENSE" "$root/NOTICE" "$stage/"
		cp "$root/dist/install.sh" "$stage/install.sh"
		case "$plat" in
		linux_*) cp "$root/dist/linux/nu.service" "$root/dist/linux/nu-user.service" "$stage/" ;;
		darwin_*) cp "$root/dist/macos/io.ipfs.nu.daemon.plist" "$stage/" ;;
		esac
		chmod -R u=rwX,go=rX "$stage"
		chmod 0755 "$stage/nu" "$stage/install.sh"
		# Reproducible: fixed order, owner and mtime (SOURCE_DATE_EPOCH or 2000-01-01).
		mtime=${SOURCE_DATE_EPOCH:-946684800}
		find "$stage" -exec touch -h -d "@$mtime" {} + 2>/dev/null || true
		tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$mtime" \
			-C "$out/stage" -cf - "$name" | gzip -n -9 >"$name.tar.gz"
		echo "packaged $out/$name.tar.gz"
	done
	rm -rf "$out/stage"
	sha256sum nu_"${version}"_*.tar.gz >SHA256SUMS
	cat SHA256SUMS
	;;
*)
	echo "release.sh: unknown command $cmd (build or package)" >&2
	exit 2
	;;
esac
