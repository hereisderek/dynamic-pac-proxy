#!/usr/bin/env bash
set -euo pipefail

# Installs dynamic-pac-proxy + its LuCI app directly onto a running OpenWRT
# router over SSH, without building/managing an .ipk. Good for first setup
# or quick iteration. For a proper opkg-managed install (uninstall/upgrade
# tracking, dependency on luci-base, etc.) use build-ipk.sh instead.
#
# Usage:
#   install-over-ssh.sh -r <router> -g <GOARCH> [-m <GOARM>] [-u <user>]
#
# Example:
#   install-over-ssh.sh -r 192.168.1.1 -g arm64

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPENWRT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROJECT_ROOT="$(cd "$OPENWRT_DIR/.." && pwd)"

ROUTER=""
SSH_USER="root"
GOARCH=""
GOARM=""

usage() {
	cat <<EOF
Usage: install-over-ssh.sh -r <router> -g <GOARCH> [-m <GOARM>] [-u <user>]

  -r <router>  Required. Router hostname or IP, reachable over SSH.
  -g <GOARCH>  Required. Go GOARCH for the router's CPU (see ../../README.md's
               "OpenWRT routers" section for a chipset-to-GOARCH table).
  -m <GOARM>   GOARM, only meaningful when -g arm. Defaults to 7.
  -u <user>    SSH user. Defaults to root.

Example:
  install-over-ssh.sh -r 192.168.1.1 -g arm64
EOF
}

while getopts "r:g:m:u:h" opt; do
	case "$opt" in
	r) ROUTER="$OPTARG" ;;
	g) GOARCH="$OPTARG" ;;
	m) GOARM="$OPTARG" ;;
	u) SSH_USER="$OPTARG" ;;
	h) usage; exit 0 ;;
	*) usage; exit 1 ;;
	esac
done

if [ -z "$ROUTER" ] || [ -z "$GOARCH" ]; then
	echo "error: -r <router> and -g <GOARCH> are required" >&2
	usage
	exit 1
fi
if [ "$GOARCH" = "arm" ] && [ -z "$GOARM" ]; then
	GOARM="7"
fi

TARGET="$SSH_USER@$ROUTER"
CONFFILE="etc/config/dynamic-pac-proxy"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> Building for GOARCH=$GOARCH${GOARM:+ GOARM=$GOARM}"
(
	cd "$PROJECT_ROOT"
	env GOOS=linux GOARCH="$GOARCH" ${GOARM:+GOARM="$GOARM"} \
		go build -ldflags="-s -w" -o "$WORK/dynamic-pac-proxy" .
)

echo "==> Copying app files to $TARGET (except an existing UCI config)"
tar czf "$WORK/files.tar.gz" -C "$OPENWRT_DIR/files" --exclude="./$CONFFILE" .
scp -q "$WORK/files.tar.gz" "$TARGET:/tmp/dynamic-pac-proxy-files.tar.gz"
ssh "$TARGET" 'tar xzf /tmp/dynamic-pac-proxy-files.tar.gz -C / && rm -f /tmp/dynamic-pac-proxy-files.tar.gz'

if ssh "$TARGET" "[ -f /$CONFFILE ]"; then
	echo "==> /$CONFFILE already exists on the router, leaving it as-is"
else
	echo "==> installing default UCI config"
	scp -q "$OPENWRT_DIR/files/$CONFFILE" "$TARGET:/$CONFFILE"
fi

echo "==> Copying binary"
scp -q "$WORK/dynamic-pac-proxy" "$TARGET:/usr/bin/dynamic-pac-proxy.new"
ssh "$TARGET" '
	set -e
	chmod 0755 /usr/bin/dynamic-pac-proxy.new
	mv -f /usr/bin/dynamic-pac-proxy.new /usr/bin/dynamic-pac-proxy
	chmod 0755 /etc/init.d/dynamic-pac-proxy /usr/lib/dynamic-pac-proxy/uci2yaml.sh
	/etc/init.d/dynamic-pac-proxy enable
	/etc/init.d/dynamic-pac-proxy restart
'

echo "==> Done. Check with:"
echo "    ssh $TARGET '/etc/init.d/dynamic-pac-proxy status; logread | grep dynamic-pac-proxy'"
echo "Then open LuCI: Services > Dynamic PAC Proxy."
