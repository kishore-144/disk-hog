#!/bin/sh
# disk-hog — one-command pull, build and deploy.
#
#   ./deploy.sh                 pull (if a git remote exists), build, install, (re)start
#   ./deploy.sh --port 8080     ...and serve on another port (default 6969)
#   ./deploy.sh --system        install the binary to /usr/local/bin (uses sudo)
#   ./deploy.sh --linger        keep the service running when you are logged out (systemd)
#   ./deploy.sh --no-pull | --no-build | --no-service
#   ./deploy.sh status | logs | restart | stop | uninstall [--purge]
#
# Works on any Linux distribution (systemd, OpenRC, runit, s6, WSL, containers)
# and on macOS and FreeBSD. Installs per user: no root needed. Builds with Go
# 1.21+ when available; otherwise uses the prebuilt binary shipped next to this
# script by `make dist`.
set -eu

APP=disk-hog
HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"

if [ -t 1 ]; then B='\033[1m'; C='\033[1;35m'; Y='\033[1;33m'; R='\033[1;31m'; N='\033[0m'; else B= C= Y= R= N=; fi
say()  { printf "${C}==>${N} ${B}%s${N}\n" "$*"; }
info() { printf "    %s\n" "$*"; }
warn() { printf "${Y}warning:${N} %s\n" "$*" >&2; }
die()  { printf "${R}error:${N} %s\n" "$*" >&2; exit 1; }

ACTION=deploy PULL=1 BUILD=1 SERVICE=1 SYSTEM=0 LINGER=0 PURGE=0 PORT=""
while [ $# -gt 0 ]; do
	case "$1" in
	deploy|status|logs|restart|stop|uninstall) ACTION=$1 ;;
	--no-pull) PULL=0 ;;
	--no-build) BUILD=0 ;;
	--no-service) SERVICE=0 ;;
	--system) SYSTEM=1 ;;
	--linger) LINGER=1 ;;
	--purge) PURGE=1 ;;
	--port) shift; PORT=${1:-}; [ -n "$PORT" ] || die "--port needs a value" ;;
	--port=*) PORT=${1#--port=} ;;
	[0-9]*) PORT=$1 ;;
	-h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
	shift
done

# Validate before touching anything, so a typo changes nothing.
if [ -n "$PORT" ]; then
	case "$PORT" in *[!0-9]*) die "invalid port: $PORT (must be a number from 1 to 65535)" ;; esac
	[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "invalid port: $PORT (must be a number from 1 to 65535)"
fi

# ---- platform -------------------------------------------------------------------
case "$(uname -s)" in
	Linux) GOOS=linux ;;
	Darwin) GOOS=darwin ;;
	FreeBSD) GOOS=freebsd ;;
	OpenBSD) GOOS=openbsd ;;
	NetBSD) GOOS=netbsd ;;
	*) die "unsupported OS $(uname -s); on Windows use deploy.ps1" ;;
esac
case "$(uname -m)" in
	x86_64|amd64) GOARCH=amd64 ;;
	aarch64|arm64) GOARCH=arm64 ;;
	armv7*|armv8l) GOARCH=arm GOARM=7 ;;
	armv6*) GOARCH=arm GOARM=6 ;;
	i?86) GOARCH=386 ;;
	riscv64) GOARCH=riscv64 ;;
	ppc64le) GOARCH=ppc64le ;;
	s390x) GOARCH=s390x ;;
	loongarch64) GOARCH=loong64 ;;
	mips64el) GOARCH=mips64le ;;
	*) GOARCH=$(uname -m) ;;
esac

if [ "$SYSTEM" = 1 ]; then BIN_DIR=/usr/local/bin; else BIN_DIR="$HOME/.local/bin"; fi
BIN="$BIN_DIR/$APP"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/$APP"
[ "$GOOS" = darwin ] && STATE_DIR="$HOME/Library/Application Support/$APP"
LOG="$STATE_DIR/$APP.log"
PIDFILE="$STATE_DIR/$APP.pid"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
PLIST="$HOME/Library/LaunchAgents/local.$APP.plist"
AUTOSTART="${XDG_CONFIG_HOME:-$HOME/.config}/autostart/$APP.desktop"

if [ "$(id -u)" = 0 ] && [ "$SYSTEM" = 0 ] && [ "$ACTION" = deploy ]; then
	warn "running as root installs disk-hog for root, which then scans root's home."
	warn "run ./deploy.sh as your normal user (add --system to put the binary in /usr/local/bin)."
fi

SUDO=""
if [ "$SYSTEM" = 1 ] && [ "$(id -u)" != 0 ]; then
	command -v sudo >/dev/null 2>&1 && SUDO=sudo || { command -v doas >/dev/null 2>&1 && SUDO=doas || die "--system needs sudo or doas"; }
fi

# Which service manager can we use as this user?
manager() {
	if [ "$GOOS" = darwin ]; then echo launchd; return; fi
	if [ "$GOOS" = linux ] && command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
		echo systemd; return
	fi
	echo nohup
}
MGR=$(manager)

# ---- steps ------------------------------------------------------------------------
pull() {
	[ "$PULL" = 1 ] || return 0
	[ -d .git ] && command -v git >/dev/null 2>&1 || return 0
	if [ -z "$(git remote 2>/dev/null)" ]; then
		info "no git remote configured; building the local checkout"
		return 0
	fi
	say "Pulling latest changes"
	git pull --ff-only || die "git pull failed (local changes or diverged branch). Fix it or rerun with --no-pull."
}

find_go() {
	for g in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go "$HOME/.local/go/bin/go" "$HOME/go/bin/go" \
		/usr/lib/go/bin/go /usr/lib/golang/bin/go /snap/bin/go /opt/homebrew/bin/go /usr/local/bin/go; do
		[ -n "$g" ] && [ -x "$g" ] || continue
		v=$("$g" env GOVERSION 2>/dev/null | sed 's/^go//')
		major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
		case "$major$minor" in *[!0-9]*|"") continue ;; esac
		if [ "$major" -gt 1 ] || [ "$minor" -ge 21 ]; then echo "$g"; return 0; fi
	done
	return 1
}

SRC=""
build() {
	if [ "$BUILD" = 0 ]; then
		[ -x "build/$APP" ] || die "--no-build given but build/$APP does not exist"
		SRC="build/$APP"; return 0
	fi
	if [ -f go.mod ] && GO=$(find_go); then
		VERSION=$(git describe --tags --always --dirty 2>/dev/null || cat VERSION 2>/dev/null || echo dev)
		say "Building $APP $VERSION with $("$GO" version | awk '{print $3}') for $GOOS/$GOARCH"
		mkdir -p build
		# GOTOOLCHAIN=local: never download a toolchain; GOFLAGS=-mod=mod is
		# harmless since there are no third-party dependencies.
		CGO_ENABLED=0 GOTOOLCHAIN=local GOOS=$GOOS GOARCH=$GOARCH GOARM=${GOARM:-} \
			"$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "build/$APP.tmp" ./cmd/$APP
		mv -f "build/$APP.tmp" "build/$APP"
		SRC="build/$APP"
	elif [ -x "./$APP" ] && [ ! -d "./$APP" ]; then
		say "Go not found; using the bundled binary"
		SRC="./$APP"
	elif [ -x "dist/$APP-$GOOS-$GOARCH" ]; then
		say "Go not found; using dist/$APP-$GOOS-$GOARCH"
		SRC="dist/$APP-$GOOS-$GOARCH"
	else
		die "Go 1.21+ is not installed and there is no prebuilt binary for $GOOS/$GOARCH.
       Install Go (your package manager, or https://go.dev/dl), or on a machine with Go run
       'make dist' and copy dist/$APP-$GOOS-$GOARCH.tar.gz here."
	fi
	"$SRC" version >/dev/null 2>&1 || die "$SRC does not run on this machine ($GOOS/$GOARCH)"
}

install_bin() {
	say "Installing $BIN"
	$SUDO mkdir -p "$BIN_DIR"
	# Copy then rename: replacing the file atomically works even while the
	# old binary is running.
	$SUDO cp "$SRC" "$BIN.new"
	$SUDO chmod 755 "$BIN.new"
	$SUDO mv -f "$BIN.new" "$BIN"
	case ":$PATH:" in
		*":$BIN_DIR:"*) ;;
		*) warn "$BIN_DIR is not on your PATH. Add this to your shell profile:"
		   info "export PATH=\"$BIN_DIR:\$PATH\"" ;;
	esac
}

configure() {
	"$BIN" config init >/dev/null
	if [ -n "$PORT" ]; then
		"$BIN" config set port "$PORT" >/dev/null
		info "port set to $PORT"
	fi
	info "config: $("$BIN" config path)"
}

render() { sed -e "s|@BIN@|$BIN|g" -e "s|@LOG@|$LOG|g" "$1"; }

stop_nohup() {
	if [ -f "$PIDFILE" ]; then
		pid=$(cat "$PIDFILE" 2>/dev/null || true)
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			i=0; while kill -0 "$pid" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
		fi
		rm -f "$PIDFILE"
	fi
}

start_service() {
	[ "$SERVICE" = 1 ] || { info "skipping service setup (--no-service). Run: $BIN serve"; return 0; }
	mkdir -p "$STATE_DIR"
	case "$MGR" in
	systemd)
		say "Starting systemd user service"
		mkdir -p "$UNIT_DIR"
		render packaging/$APP.service > "$UNIT_DIR/$APP.service"
		systemctl --user daemon-reload
		systemctl --user enable "$APP.service" >/dev/null 2>&1
		systemctl --user restart "$APP.service"
		if [ "$LINGER" = 1 ]; then
			loginctl enable-linger "$(id -un)" && info "linger enabled: runs even when you are logged out"
		fi
		;;
	launchd)
		say "Starting launchd agent"
		mkdir -p "$(dirname "$PLIST")"
		render packaging/local.$APP.plist > "$PLIST"
		launchctl bootout "gui/$(id -u)/local.$APP" 2>/dev/null || true
		launchctl bootstrap "gui/$(id -u)" "$PLIST"
		;;
	nohup)
		say "Starting in the background (no systemd user session found)"
		stop_nohup
		nohup "$BIN" serve >> "$LOG" 2>&1 &
		echo $! > "$PIDFILE"
		if [ -n "${XDG_CURRENT_DESKTOP:-}${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
			mkdir -p "$(dirname "$AUTOSTART")"
			render packaging/$APP.desktop > "$AUTOSTART"
			info "autostart entry: $AUTOSTART"
		else
			info "to start at boot, add to 'crontab -e':  @reboot $BIN serve >> $LOG 2>&1"
		fi
		;;
	esac
}

health() {
	[ "$SERVICE" = 1 ] || return 0
	if out=$("$BIN" health --wait 10s 2>&1); then
		url=$(printf '%s' "$out" | sed -n 's/.* at \(http[^ ]*\).*/\1/p')
		say "disk-hog is running → ${url:-$out}"
	else
		printf '%s\n' "$out" >&2
		show_logs 30
		die "service did not come up"
	fi
}

show_logs() {
	n=${1:-100}
	case "$MGR" in
		systemd) journalctl --user -u "$APP" -n "$n" --no-pager ${FOLLOW:-} ;;
		*) if [ -f "$LOG" ]; then tail -n "$n" ${FOLLOW:-} "$LOG"; fi ;;
	esac
}

# ---- actions ----------------------------------------------------------------------
case "$ACTION" in
deploy)
	pull
	build
	install_bin
	configure
	start_service
	health
	info "CLI:  $APP scan | $APP clean --help | $APP system"
	info "Logs: ./deploy.sh logs     Status: ./deploy.sh status"
	;;
status)
	case "$MGR" in
		systemd) systemctl --user --no-pager status "$APP" 2>/dev/null | head -12 || true ;;
		launchd) launchctl print "gui/$(id -u)/local.$APP" 2>/dev/null | grep -E 'state|pid' || true ;;
		nohup) if [ -f "$PIDFILE" ]; then info "pid $(cat "$PIDFILE")"; fi ;;
	esac
	"$BIN" health || exit 1
	;;
logs)
	FOLLOW=-f show_logs 100
	;;
restart)
	case "$MGR" in
		systemd) systemctl --user restart "$APP" ;;
		launchd) launchctl kickstart -k "gui/$(id -u)/local.$APP" ;;
		nohup) stop_nohup; mkdir -p "$STATE_DIR"; nohup "$BIN" serve >> "$LOG" 2>&1 & echo $! > "$PIDFILE" ;;
	esac
	health
	;;
stop)
	case "$MGR" in
		systemd) systemctl --user stop "$APP" ;;
		launchd) launchctl bootout "gui/$(id -u)/local.$APP" 2>/dev/null || true ;;
		nohup) stop_nohup ;;
	esac
	say "stopped"
	;;
uninstall)
	say "Uninstalling $APP"
	if [ "$MGR" = systemd ]; then
		systemctl --user disable --now "$APP.service" 2>/dev/null || true
		rm -f "$UNIT_DIR/$APP.service"
		systemctl --user daemon-reload 2>/dev/null || true
	fi
	if [ "$GOOS" = darwin ]; then
		launchctl bootout "gui/$(id -u)/local.$APP" 2>/dev/null || true
		rm -f "$PLIST"
	fi
	stop_nohup
	rm -f "$AUTOSTART"
	for b in "$HOME/.local/bin/$APP" "/usr/local/bin/$APP"; do
		if [ -e "$b" ]; then
			if [ -w "$(dirname "$b")" ]; then rm -f "$b"; else ${SUDO:-sudo} rm -f "$b"; fi
			info "removed $b"
		fi
	done
	if [ "$PURGE" = 1 ]; then
		cfg_dir="${XDG_CONFIG_HOME:-$HOME/.config}/$APP"
		[ "$GOOS" = darwin ] && cfg_dir="$HOME/Library/Application Support/$APP"
		rm -rf "$cfg_dir" "$STATE_DIR"
		info "removed configuration and state"
	else
		info "kept configuration and history (use --purge to remove them)"
	fi
	;;
esac
