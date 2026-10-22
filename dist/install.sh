#!/bin/sh
# Install or upgrade Nu from a release tarball (or a bare binary) on Linux or macOS.
#
#   install.sh [--user | --system | --no-service] [--prefix DIR] [--backup DIR] <nu_*.tar.gz | nu>
#
#   --user        (default) per-user daemon: ~/.local/bin/nu (Linux) or /usr/local/bin/nu
#                 (macOS), systemd user unit or launchd LaunchAgent, state in ~/.nu
#   --system      Linux servers, as root: /usr/local/bin/nu, `nu` system user, state in
#                 /var/lib/nu (NU_PATH), systemd unit nu.service
#   --no-service  only install the binary
#   --prefix DIR  install the binary into DIR instead
#   --backup DIR  before an upgrade, with the daemon stopped, write `nu repo backup` of
#                 the existing state into DIR (keys in plaintext, mode 0600)
#
# An upgrade stops the daemon, replaces the binary atomically and starts it again; the
# new daemon migrates the store forward on start. Downgrades are not supported: an older
# binary refuses a store a newer one migrated (docs/operations.md "Upgrades and
# compatibility"). A tarball is checked against SHA256SUMS when that file sits beside it.
set -eu

mode=user
prefix=
backup=
src=
while [ $# -gt 0 ]; do
	case "$1" in
	--user) mode=user ;;
	--system) mode=system ;;
	--no-service) mode=none ;;
	--prefix) shift; prefix=$1 ;;
	--backup) shift; backup=$1 ;;
	-h|--help) sed -n '2,20p' "$0"; exit 0 ;;
	-*) echo "install.sh: unknown flag $1" >&2; exit 2 ;;
	*) src=$1 ;;
	esac
	shift
done
[ -n "$src" ] || { echo "install.sh: give a nu_*.tar.gz release tarball or a nu binary" >&2; exit 2; }
[ -e "$src" ] || { echo "install.sh: $src does not exist" >&2; exit 2; }

os=$(uname -s)
say() { echo "install.sh: $*"; }
die() { echo "install.sh: $*" >&2; exit 1; }

if [ "$mode" = system ]; then
	[ "$os" = Linux ] || die "--system is for Linux servers; use --user on macOS"
	[ "$(id -u)" = 0 ] || die "--system must run as root (sudo)"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

case "$src" in
*.tar.gz|*.tgz)
	sums=$(dirname "$src")/SHA256SUMS
	if [ -f "$sums" ]; then
		name=$(basename "$src")
		want=$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$sums")
		[ -n "$want" ] || die "$name is not listed in $sums"
		if command -v sha256sum >/dev/null 2>&1; then
			got=$(sha256sum "$src" | awk '{print $1}')
		else
			got=$(shasum -a 256 "$src" | awk '{print $1}')
		fi
		[ "$got" = "$want" ] || die "$name does not match SHA256SUMS (got $got)"
		say "checksum ok ($name)"
	else
		say "warning: no SHA256SUMS beside $src; checksum not verified"
	fi
	tar -xzf "$src" -C "$tmp"
	new=$(find "$tmp" -type f -name nu | head -n 1)
	dist=$(dirname "$new")
	;;
*)
	new=$tmp/nu
	cp "$src" "$new"
	dist=$(cd "$(dirname "$0")" && pwd)
	;;
esac
[ -n "$new" ] && [ -f "$new" ] || die "no nu binary in $src"
# distfile finds a service definition in the tarball or in the source tree's dist/.
distfile() {
	for d in "$dist" "$dist/linux" "$dist/macos"; do
		[ -f "$d/$1" ] && { echo "$d/$1"; return; }
	done
	die "$1 not found beside the binary (use a release tarball or run from dist/)"
}
chmod 0755 "$new"
newver=$("$new" version) || die "the new binary does not run on this machine ($os $(uname -m))"
say "installing $(echo "$newver" | head -n 1)"

if [ -z "$prefix" ]; then
	case "$mode" in
	system) prefix=/usr/local/bin ;;
	*) if [ "$os" = Darwin ]; then prefix=/usr/local/bin; else prefix=$HOME/.local/bin; fi ;;
	esac
fi
mkdir -p "$prefix"
bin=$prefix/nu

# Service handles.
nupath=
case "$mode" in
system) nupath=/var/lib/nu ;;
user) nupath=${NU_PATH:-$HOME/.nu} ;;
esac
run_nu() { # run the installed CLI against the service's repo
	if [ "$mode" = system ]; then
		runuser -u nu -- env NU_PATH="$nupath" "$bin" "$@"
	else
		NU_PATH="$nupath" "$bin" "$@"
	fi
}
plist=$HOME/Library/LaunchAgents/io.ipfs.nu.daemon.plist
stop_service() {
	case "$mode/$os" in
	system/Linux) systemctl stop nu.service 2>/dev/null || true ;;
	user/Linux) systemctl --user stop nu.service 2>/dev/null || true ;;
	user/Darwin) launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true ;;
	esac
}

upgrade=no
if [ -x "$bin" ]; then
	upgrade=yes
	say "upgrading from $("$bin" version 2>/dev/null | head -n 1 || echo 'an unknown version')"
	stop_service
	if [ -n "$backup" ] && [ "$mode" != none ] && [ -d "$nupath" ]; then
		if [ "$mode" = system ]; then
			install -d -m 0700 -o nu -g nu "$backup"
		else
			mkdir -p "$backup"
		fi
		out=$backup/nu-authority-$(date +%Y%m%dT%H%M%S).tar
		run_nu repo backup "$out" || die "pre-upgrade backup failed; nothing was changed"
		say "backed up keys and cluster state to $out"
	fi
fi

# Atomic replace: a running shell or daemon keeps the old inode.
cp "$new" "$prefix/.nu.new"
chmod 0755 "$prefix/.nu.new"
mv -f "$prefix/.nu.new" "$bin"
say "installed $bin"

case "$mode/$os" in
none/*)
	;;
system/Linux)
	if ! id nu >/dev/null 2>&1; then
		useradd --system --home-dir /var/lib/nu --no-create-home --shell /usr/sbin/nologin nu
		say "created system user nu"
	fi
	install -d -m 0700 -o nu -g nu /var/lib/nu
	sed "s#/usr/local/bin/nu#$bin#" "$(distfile nu.service)" >/etc/systemd/system/nu.service
	systemctl daemon-reload
	systemctl enable nu.service >/dev/null
	systemctl restart nu.service
	;;
user/Linux)
	unit=$HOME/.config/systemd/user/nu.service
	mkdir -p "$(dirname "$unit")"
	sed "s#%h/.local/bin/nu#$bin#" "$(distfile nu-user.service)" >"$unit"
	systemctl --user daemon-reload
	systemctl --user enable nu.service >/dev/null
	systemctl --user restart nu.service
	;;
user/Darwin)
	logs=$HOME/Library/Logs
	mkdir -p "$(dirname "$plist")" "$logs"
	sed -e "s#@BIN@#$bin#" -e "s#@LOGS@#$logs#" "$(distfile io.ipfs.nu.daemon.plist)" >"$plist"
	launchctl bootstrap "gui/$(id -u)" "$plist"
	;;
*)
	die "no service definition for $mode on $os; rerun with --no-service"
	;;
esac

if [ "$mode" != none ]; then
	i=0
	# `operations` only talks to a running daemon (status would fall back to an in-process
	# node and take the repo lock from the starting daemon).
	until run_nu operations >/dev/null 2>&1; do
		i=$((i + 1))
		if [ $i -ge 60 ]; then
			echo "install.sh: the daemon did not come up; see its log (journalctl -u nu / ~/Library/Logs/nu.log)." >&2
			echo "install.sh: a store migrated by a newer release is refused by an older one: reinstall the newer release." >&2
			exit 1
		fi
		sleep 0.5
	done
	say "daemon running ($([ $upgrade = yes ] && echo upgraded || echo installed)); state in $nupath"
fi
