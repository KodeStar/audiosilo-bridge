#!/bin/sh
# Install an AudioSilo Bridge ASR worker on this computer (macOS or Linux).
#
# Served by your Bridge, with its address below. Run it the way the Bridge's
# Workers tab shows:
#
#   curl -fsSL <bridge>/worker/install.sh | sh -s -- --token asw_...
#
# It downloads the Bridge's own audiosilo-bridge build for this computer from
# the Bridge, puts it in ~/.audiosilo-bridge-worker/bin (or --dir), then
# installs the worker as a service that starts at login (launchd on macOS, a
# systemd user unit on Linux), or with --run runs it in this terminal instead.
#
#   --token <asw_...>  the worker's token from the Workers tab (required the
#                      first time; running it again to update reuses the saved one)
#   --run              run the worker here in the foreground, no service
#   --dir <path>       where to put the program
#
# A file curl downloads carries no quarantine mark, so macOS Gatekeeper
# doesn't stop the unsigned program. The script holds no secret: the token
# comes from your command line.
set -eu

BRIDGE='@@BRIDGE@@'
RELEASES='https://github.com/KodeStar/audiosilo-bridge/releases'

say() { printf '%s\n' "$*"; }
usage() {
	say 'Usage: install.sh [--token <asw_...>] [--run] [--dir <path>]'
	say '  --token <asw_...>  the worker token from the Bridge page (Workers), required the first time'
	say '  --run              run the worker here in the foreground instead of installing the service'
	say '  --dir <path>       where to put the program (default ~/.audiosilo-bridge-worker/bin)'
}
die() {
	printf 'audiosilo-bridge worker: %s\n' "$*" >&2
	exit 1
}

# main runs only once the whole script has arrived: a download cut short runs
# nothing, and no command it starts can read the rest of the script from the pipe.
main() {
	token=''
	run=0
	dir="${HOME:-}/.audiosilo-bridge-worker/bin"
	while [ $# -gt 0 ]; do
		case "$1" in
		--token)
			[ $# -ge 2 ] || die '--token needs a value'
			token=$2
			shift 2
			;;
		--token=*)
			token=${1#--token=}
			shift
			;;
		--run)
			run=1
			shift
			;;
		--dir)
			[ $# -ge 2 ] || die '--dir needs a value'
			dir=$2
			shift 2
			;;
		--dir=*)
			dir=${1#--dir=}
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $1 (it takes --token, --run and --dir)" ;;
		esac
	done

	if [ -z "$token" ]; then
		# An update: the worker keeps its token in its data dir (as `worker`
		# finds it: $AUDIOSILO_WORKER_DATA, else ~/.audiosilo-bridge-worker), or
		# takes $AUDIOSILO_WORKER_TOKEN.
		data="${AUDIOSILO_WORKER_DATA:-${HOME:-}/.audiosilo-bridge-worker}"
		[ -n "${AUDIOSILO_WORKER_TOKEN:-}" ] || [ -s "$data/worker.token" ] ||
			die "--token is required: add a worker on the Bridge's page (Workers) and copy its command"
	fi
	[ -n "$dir" ] || die '--dir is empty'

	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "this computer runs $(uname -s): a worker runs on macOS or Linux only" ;;
	esac
	case "$(uname -m)" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) die "this computer is $(uname -m): a worker runs on arm64 or amd64 only" ;;
	esac
	# A terminal under Rosetta says x86_64 on an Apple Silicon Mac: take the
	# native build, which is the one with Metal.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi

	command -v curl >/dev/null 2>&1 || die 'curl is needed'
	command -v gzip >/dev/null 2>&1 || die 'gzip is needed'

	mkdir -p "$dir" || die "can't create $dir"
	bin="$dir/audiosilo-bridge"
	tmp="$dir/.audiosilo-bridge.download.$$"
	trap 'rm -f "$tmp" "$tmp.gz"' EXIT
	trap 'exit 1' HUP INT TERM

	say "Downloading the worker for $os/$arch from $BRIDGE ..."
	curl -fsSL -o "$tmp.gz" "$BRIDGE/worker/download/$os/$arch" ||
		die "the download failed. Is $BRIDGE reachable from here? Or download audiosilo-bridge for $os/$arch from $RELEASES"
	gzip -dc "$tmp.gz" >"$tmp" || die 'the download is damaged: try again'
	rm -f "$tmp.gz"
	chmod 755 "$tmp"
	version=$("$tmp" version) || die "the downloaded program doesn't run on this computer"
	mv -f "$tmp" "$bin"
	say "Installed audiosilo-bridge $version at $bin"
	case ":${PATH:-}:" in
	*":$dir:"*) ;;
	*) say "To run it by name, add its folder to your PATH: export PATH=\"$dir:\$PATH\"" ;;
	esac

	# The worker reads a saved token itself when --token isn't given.
	set -- --bridge "$BRIDGE"
	if [ -n "$token" ]; then
		set -- "$@" --token "$token"
	fi
	if [ "$run" = 1 ]; then
		say 'Starting the worker here (Ctrl-C stops it) ...'
		exec "$bin" worker "$@"
	fi
	"$bin" worker install "$@"
}

main "$@"
