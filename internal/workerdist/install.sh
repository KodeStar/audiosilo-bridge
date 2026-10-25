#!/bin/sh
# Install an AudioSilo Bridge ASR worker on this computer (macOS or Linux).
#
# Served by your Bridge, with its address below. Run it the way the Bridge's
# Workers tab shows:
#
#   curl -fsSL <bridge>/worker/install.sh | sh -s -- --token asw_...
#
# It downloads the Bridge's own audiosilo-bridge build for this computer from
# the Bridge, puts it in ~/.audiosilo-bridge-worker/bin (or --dir; an update
# without --dir reuses the installed service's folder), then installs the
# worker as a service that starts at login (launchd on macOS, a systemd user
# unit on Linux; an update keeps the service's settings, --data included), or
# with --run runs it in this terminal instead.
#
#   --token <asw_...>  the worker's token from the Workers tab (required the
#                      first time; running it again to update reuses the saved one)
#   --run              run the worker here in the foreground, no service (a
#                      --run start doesn't save the token)
#   --dir <path>       where to put the program (default: the installed
#                      service's folder, else ~/.audiosilo-bridge-worker/bin)
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
	say '                     (it does not save a --token: a later run needs it again)'
	say '  --dir <path>       where to put the program (default: the installed service'"'"'s folder,'
	say '                     else ~/.audiosilo-bridge-worker/bin)'
}
die() {
	printf 'audiosilo-bridge worker: %s\n' "$*" >&2
	exit 1
}

# service_args prints the installed worker service's command line, one word a
# line, as `worker install` writes it (the launchd agent's ProgramArguments,
# the systemd user unit's first ExecStart: "quoted" words, or bare); nothing
# when there is none. A word the file had to escape comes out escaped (the
# callers refuse those), and a unit's words stop at the first escape or open
# quote, since what follows can't be read plainly.
service_args() {
	[ -n "${HOME:-}" ] || return 0
	case "$os" in
	darwin)
		f="$HOME/Library/LaunchAgents/app.audiosilo.bridge-worker.plist"
		[ -f "$f" ] || return 0
		awk '/<key>ProgramArguments<\/key>/ { args = 1; next }
			args && /<\/array>/ { exit }
			args && /<string>/ { sub(/^[ \t]*<string>/, ""); sub(/<\/string>[ \t]*$/, ""); print }' "$f" 2>/dev/null || true
		;;
	linux)
		f="$HOME/.config/systemd/user/audiosilo-bridge-worker.service"
		[ -f "$f" ] || return 0
		awk '/^[ \t]*ExecStart=/ {
			line = $0
			sub(/^[ \t]*ExecStart=[ \t]*/, "", line)
			while (line != "") {
				if (match(line, /^"[^"]*"/)) word = substr(line, 2, RLENGTH - 2)
				else if (match(line, /^[^" \t]+/)) word = substr(line, 1, RLENGTH)
				else exit
				if (index(word, "\\")) exit
				print word
				line = substr(line, RLENGTH + 1)
				sub(/^[ \t]+/, "", line)
			}
			exit
		}' "$f" 2>/dev/null || true
		;;
	esac
}

# plain prints $1 when it is an absolute path the service file holds as is
# (nothing it had to escape: \ % $ & < >), else nothing.
plain() {
	case "$1" in
	*[\\%\$\&\<\>]*) ;;
	/*) printf '%s\n' "$1" ;;
	esac
}

# installed_dir prints the folder of the program the installed worker service
# runs; nothing when there is none or it doesn't read as an existing absolute
# folder this user can write (a path the file had to escape included: pass
# --dir then).
installed_dir() {
	prog=$(plain "$(service_args | head -n 1)")
	case "$prog" in
	*/) return 0 ;;
	/*/*) ;;
	*) return 0 ;;
	esac
	prog=${prog%/*}
	# One this user can't write (a program moved to /usr/local/bin by hand)
	# leaves the default, which `worker install` then points the service at.
	[ -d "$prog" ] && [ -w "$prog" ] && printf '%s\n' "$prog"
	return 0
}

# installed_data prints the installed service's --data folder (--data <dir>
# or --data=<dir> after the program's `worker`), when it reads plainly.
installed_data() {
	plain "$(service_args | awk 'NR == 1 { next } take { print; exit }
		$0 == "--data" || $0 == "-data" { take = 1; next }
		/^--?data=/ { sub(/^--?data=/, ""); print; exit }')"
}

# main runs only once the whole script has arrived: a download cut short runs
# nothing, and no command it starts can read the rest of the script from the pipe.
main() {
	token=''
	run=0
	dir=''
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
			[ -n "$2" ] || die '--dir is empty'
			dir=$2
			shift 2
			;;
		--dir=*)
			dir=${1#--dir=}
			[ -n "$dir" ] || die '--dir is empty'
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $1 (it takes --token, --run and --dir)" ;;
		esac
	done

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

	if [ -z "$token" ]; then
		# An update: the worker keeps its token in its data dir, or takes
		# $AUDIOSILO_WORKER_TOKEN. `worker install` keeps the installed
		# service's --data, so its folder is the one to look in; `worker` (a
		# --run start) uses $AUDIOSILO_WORKER_DATA, else ~/.audiosilo-bridge-worker.
		data=''
		[ "$run" = 1 ] || data=$(installed_data)
		[ -n "$data" ] || data="${AUDIOSILO_WORKER_DATA:-${HOME:-}/.audiosilo-bridge-worker}"
		[ -n "${AUDIOSILO_WORKER_TOKEN:-}" ] || [ -s "$data/worker.token" ] ||
			die "--token is required: add a worker on the Bridge's page (Workers) and copy its command"
	fi

	# An update with no --dir goes where the installed service runs it from.
	svcdir=$(installed_dir)
	if [ -z "$dir" ]; then
		dir=$svcdir
		if [ -n "$dir" ]; then
			say "Updating the worker in $dir, where its service runs it from (--dir puts it elsewhere)"
		else
			dir="${HOME:-}/.audiosilo-bridge-worker/bin"
		fi
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
		# The installed service runs the program just replaced, but goes on
		# with the old one it loaded until it restarts.
		if [ -n "$svcdir" ] && [ "$svcdir" = "$dir" ]; then
			case "$os" in
			darwin) restart="launchctl kickstart -k gui/$(id -u)/app.audiosilo.bridge-worker" ;;
			*) restart='systemctl --user restart audiosilo-bridge-worker.service' ;;
			esac
			say "The installed worker service runs the program in $dir too: it keeps the old one until it restarts."
			say "To update it now: $restart (or run this again without --run)."
		fi
		say 'Starting the worker here (Ctrl-C stops it) ...'
		if [ -n "$token" ]; then
			say 'A --run start does not save the token: to run it again, pass --token again (or leave out --run to install the service, which saves it).'
		fi
		exec "$bin" worker "$@"
	fi
	"$bin" worker install "$@"
}

main "$@"
