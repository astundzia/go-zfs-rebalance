#!/bin/sh
# Installer for go-zfs-rebalance (the "rebalance" command).
#
#   curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash
#
# Run it with --help to see the options. It is plain POSIX sh, so "| sudo sh" works too.
# Everything below is a function that only runs from the last line, so a download
# that stops half-way never runs part of the script.

set -eu
umask 022

REPO_URL="https://github.com/astundzia/go-zfs-rebalance"
ONE_LINER="curl -fsSL $REPO_URL/releases/latest/download/install.sh | sudo bash"

# The partly-downloaded program, removed on any exit until it has been moved into place.
tmp_file=""

say() { printf '%s\n' "$@"; }

die() {
	printf '\n' >&2
	printf '%s\n' "$@" >&2
	exit 1
}

cleanup() {
	if [ -n "$tmp_file" ]; then
		rm -f "$tmp_file"
	fi
}

usage() {
	cat <<EOF
Installs go-zfs-rebalance (the "rebalance" command).

Usage:
  $ONE_LINER
  $ONE_LINER -s -- [options]
  sudo sh install.sh [options]

Options:
  --dir DIR          install into DIR instead of the default folder
  --version VERSION  install a specific release, for example v2.0.0 (default: the latest)
  --uninstall        remove rebalance (your saved progress files are kept)
  -h, --help         show this help

Environment variables (put them after sudo, e.g. "sudo INSTALL_DIR=/opt/bin bash"):
  INSTALL_DIR          same as --dir
  REBALANCE_VERSION    same as --version
  REBALANCE_BASE_URL   download from this address instead of GitHub (for testing)

Default folder:
  TrueNAS     root's home folder, usually /root/.local/bin (it survives TrueNAS updates)
  elsewhere   /usr/local/bin, or root's .local/bin if /usr/local/bin can't be used

The installer needs root. It never installs anything onto your pools.
EOF
}

parse_args() {
	while [ $# -gt 0 ]; do
		case $1 in
		--dir)
			[ $# -ge 2 ] || die "--dir needs a folder, for example: --dir /root/.local/bin"
			opt_dir=$2
			shift 2
			;;
		--dir=*)
			opt_dir=${1#*=}
			shift
			;;
		--version)
			[ $# -ge 2 ] || die "--version needs a release, for example: --version v2.0.0"
			opt_version=$2
			shift 2
			;;
		--version=*)
			opt_version=${1#*=}
			shift
			;;
		--uninstall)
			opt_uninstall=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			die "Unknown option: $1" "Run the installer with --help to see the options."
			;;
		esac
	done
}

require_root() {
	if [ "$(id -u)" -ne 0 ]; then
		die "The installer needs to run as root (with sudo), so it can put rebalance" \
			"in a system folder. Nothing was installed." \
			"" \
			"Please run it again like this:" \
			"" \
			"  $ONE_LINER" \
			"" \
			"(If you ran a downloaded install.sh, put sudo in front of the same command.)"
	fi
}

detect_platform() {
	kernel=$(uname -s)
	case $kernel in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	FreeBSD)
		die "Sorry, rebalance runs on Linux and macOS only, and this computer runs FreeBSD." \
			"TrueNAS CORE isn't supported, but TrueNAS SCALE / Community Edition is. Nothing was installed."
		;;
	*)
		die "Sorry, rebalance runs on Linux and macOS only, and this computer reports \"$kernel\"." \
			"Nothing was installed."
		;;
	esac

	machine=$(uname -m)
	case $machine in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*)
		die "Sorry, there's no rebalance build for this processor (\"$machine\")." \
			"Builds exist for 64-bit Intel/AMD (amd64) and 64-bit ARM (arm64). Nothing was installed."
		;;
	esac

	# Under Rosetta an Apple silicon Mac reports x86_64; the native build is the better fit.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
}

is_truenas() {
	[ "$(uname -s)" = Linux ] || return 1
	if [ -d /usr/share/truenas ]; then
		return 0
	fi
	if [ -f /etc/version ] && grep -qi truenas /etc/version 2>/dev/null; then
		return 0
	fi
	command -v midclt >/dev/null 2>&1
}

find_root_home() {
	rh=""
	if command -v getent >/dev/null 2>&1; then
		rh=$(getent passwd root 2>/dev/null | cut -d: -f6) || rh=""
	fi
	if [ -z "$rh" ]; then
		rh=~root # macOS has no getent; the shell looks the home folder up instead
	fi
	case $rh in
	/*) printf '%s\n' "$rh" ;;
	*) printf '%s\n' /root ;;
	esac
}

# can_write reports whether a file can really be created in $1. "test -w" isn't enough:
# some shells answer "yes" for root even on a read-only filesystem.
can_write() {
	probe=$(mktemp "$1/.rebalance-check.XXXXXX" 2>/dev/null) || return 1
	rm -f "$probe"
}

choose_dir() {
	dir_note=""
	if [ -n "$opt_dir" ]; then
		case $opt_dir in
		/*) install_dir=$opt_dir ;;
		*) die "Please give the folder as a full path that starts with /, for example: --dir $root_home/.local/bin" ;;
		esac
	elif is_truenas; then
		install_dir=$root_home/.local/bin
		dir_note="TrueNAS keeps the system read-only and doesn't run programs from your pools or /home, so it goes in root's home, which survives updates."
	elif [ -d /usr/local/bin ] && can_write /usr/local/bin; then
		install_dir=/usr/local/bin
	else
		install_dir=$root_home/.local/bin
		dir_note="/usr/local/bin can't be used here, so it goes in root's home folder."
	fi

	if [ "$install_dir" != / ]; then
		install_dir=${install_dir%/}
	fi

	if [ -z "$opt_dir" ]; then
		case $install_dir in
		/mnt | /mnt/*)
			die "The default folder ($install_dir) is on a pool, and rebalance is never installed there by default." \
				"Please choose a folder with --dir."
			;;
		esac
	fi
}

choose_base_url() {
	if [ -n "${REBALANCE_BASE_URL:-}" ]; then
		base_url=${REBALANCE_BASE_URL%/}
		return 0
	fi
	case $opt_version in
	"" | latest)
		base_url=$REPO_URL/releases/latest/download
		;;
	*[!A-Za-z0-9._-]*)
		die "\"$opt_version\" doesn't look like a release version. Please use something like v2.0.0."
		;;
	v*)
		base_url=$REPO_URL/releases/download/$opt_version
		;;
	*)
		base_url=$REPO_URL/releases/download/v$opt_version
		;;
	esac
}

# fetch writes the contents of URL $1 to standard output.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 "$1" </dev/null
	else
		wget -qO- "$1" </dev/null
	fi
}

have_sha256_tool() {
	command -v sha256sum >/dev/null 2>&1 ||
		command -v shasum >/dev/null 2>&1 ||
		command -v sha256 >/dev/null 2>&1
}

# sha256_of prints the lowercase SHA-256 of file $1.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1"
	else
		sha256 -q "$1"
	fi | awk '{ print tolower($1) }'
}

do_install() {
	detect_platform
	choose_base_url
	asset=rebalance-$os-$arch
	target=$install_dir/rebalance

	if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
		die "Please install curl or wget first, then run the installer again. Nothing was installed."
	fi
	if ! have_sha256_tool; then
		die "Couldn't find a tool to check the download (sha256sum or shasum)." \
			"Please install one, then run the installer again. Nothing was installed."
	fi

	say "Installing rebalance ($os/$arch) into $install_dir"
	if [ -n "$dir_note" ]; then
		say "  $dir_note"
	fi

	mkdir -p "$install_dir" 2>/dev/null ||
		die "Couldn't create the folder $install_dir. Please choose another one with --dir."
	if [ -d "$target" ]; then
		die "There's a folder called $target, so rebalance can't be put there." \
			"Please move that folder or choose another one with --dir."
	fi

	# The temp file lives in the install folder, so the final mv is a single atomic rename.
	tmp_file=$(mktemp "$install_dir/.rebalance.XXXXXXXX" 2>/dev/null) ||
		die "Couldn't write to $install_dir (it may be read-only)." \
			"Please choose another folder with --dir, for example: --dir $root_home/.local/bin"

	say "Downloading $base_url/$asset"
	fetch "$base_url/$asset" >"$tmp_file" ||
		die "The download didn't work, so nothing was installed." \
			"Please check your internet connection (and the version, if you gave one), then try again."

	say "Checking the download"
	expected=$(fetch "$base_url/$asset.sha256") ||
		die "Couldn't download the checksum file, so nothing was installed. Please try again."
	expected=$(printf '%s\n' "$expected" | awk 'NR == 1 { print tolower($1) }')
	case $expected in
	"" | *[!0-9a-f]*)
		die "The checksum file for $asset doesn't look right, so nothing was installed."
		;;
	esac
	if [ "${#expected}" -ne 64 ]; then
		die "The checksum file for $asset doesn't look right, so nothing was installed."
	fi
	actual=$(sha256_of "$tmp_file")
	if [ "$actual" != "$expected" ]; then
		die "The download doesn't match its checksum, so it was deleted and nothing was installed." \
			"This is usually a temporary network problem. Please try again in a few minutes."
	fi

	chmod 755 "$tmp_file"

	# Try the new program before it replaces anything, so a folder that can't run
	# programs (noexec) or a broken download never costs you a working copy.
	rc=0
	version_out=$("$tmp_file" --version 2>&1 </dev/null) || rc=$?
	if [ "$rc" -ne 0 ]; then
		case $version_out in
		*"ermission denied"*)
			other_dir=$root_home/.local/bin
			if [ "$other_dir" = "$install_dir" ]; then
				other_dir=/usr/local/bin
			fi
			die "The folder $install_dir can't run programs (it's probably mounted \"noexec\")," \
				"so nothing was installed. Please choose another folder, for example:" \
				"" \
				"  $ONE_LINER -s -- --dir $other_dir"
			;;
		*)
			die "The downloaded program didn't start on this computer, so nothing was installed." \
				"It said: $version_out"
			;;
		esac
	fi

	mv -f "$tmp_file" "$target" ||
		die "Couldn't move rebalance into $install_dir. Nothing was changed."
	tmp_file=""

	version_line=$(printf '%s\n' "$version_out" | head -n 1)
	say "" "Done! Installed $target"
	if [ -n "$version_line" ]; then
		say "  ($version_line)"
	fi
	say "" \
		"Next steps (swap /mnt/tank/media for one of your own folders):" \
		"" \
		"  1. See how evenly your data is spread across your drives. This only looks, it changes nothing:" \
		"       sudo $target --report /mnt/tank/media" \
		"" \
		"  2. Rebalance that folder, with a before-and-after table at the end:" \
		"       sudo $target --vdev-report /mnt/tank/media" \
		"" \
		"Tip: type \"tmux\" first and run rebalance inside it. Then closing the browser tab or" \
		"SSH session won't stop it, and \"tmux attach\" brings it back."

	case ":$PATH:" in
	*":$install_dir:"*) ;;
	*)
		say "" "Note: $install_dir isn't on the command search path, so type the full path as shown above."
		;;
	esac
	other=$(command -v rebalance 2>/dev/null || true)
	if [ -n "$other" ] && [ "$other" != "$target" ]; then
		say "" "Note: there's another copy at $other, which runs when you type just \"rebalance\"." \
			"Use the full path above, or remove the old copy."
	fi
}

do_uninstall() {
	target=$install_dir/rebalance
	if [ -e "$target" ] || [ -L "$target" ]; then
		rm -f "$target" || die "Couldn't remove $target."
		say "Removed $target."
	else
		say "rebalance isn't installed in $install_dir, so there was nothing to remove."
		if [ -z "$opt_dir" ]; then
			say "If you put it in another folder, run this again with --uninstall --dir FOLDER."
		fi
	fi
	say "" \
		"Saved progress files (if any) are in $root_home/.local/state/go-zfs-rebalance and were left in place." \
		"You can delete that folder if you won't use rebalance again."
}

main() {
	opt_dir=${INSTALL_DIR:-}
	opt_version=${REBALANCE_VERSION:-}
	opt_uninstall=0
	parse_args "$@"

	require_root

	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP

	root_home=$(find_root_home)
	choose_dir

	if [ "$opt_uninstall" = 1 ]; then
		do_uninstall
	else
		do_install
	fi
}

main "$@"
