#!/usr/bin/env bash
set -euo pipefail

# Builds an OpenWRT .ipk for dynamic-pac-proxy: the Go binary, its init
# script + default UCI config, and the LuCI app, all in one package.
# Doesn't need the OpenWRT SDK — an .ipk is just an `ar` archive of
# debian-binary + control.tar.gz + data.tar.gz, same as a .deb, and this
# assembles one by hand.
#
# Usage:
#   build-ipk.sh -a <opkg-arch> -g <GOARCH> [-m <GOARM>] [-v <version>] [-o <output-dir>]
#
# -a <opkg-arch>  Required. Must exactly match `opkg print-architecture` on
#                 the router (the non-"all" line with the highest priority),
#                 e.g. mipsel_24kc, aarch64_cortex-a53, arm_cortex-a7_neon-vfpv4.
#                 opkg refuses to install a package whose Architecture
#                 doesn't match, so this can't be guessed generically.
# -g <GOARCH>     Required. The matching Go GOARCH — see ../../README.md's
#                 "OpenWRT routers" section for a chipset-to-GOARCH table.
# -m <GOARM>      GOARM, only meaningful when -g arm (32-bit ARM). Defaults
#                 to 7; try 5 if the resulting binary fails to run.
# -v <version>    Package version. Defaults to 0.1.0.
# -o <output-dir> Where to write the .ipk. Defaults to ./dist next to this
#                 script.
#
# Example:
#   scripts/build-ipk.sh -a aarch64_cortex-a53 -g arm64 -v 1.0.0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OPENWRT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROJECT_ROOT="$(cd "$OPENWRT_DIR/.." && pwd)"

PKG_NAME="dynamic-pac-proxy"
VERSION="0.1.0"
OUT_DIR="$SCRIPT_DIR/../dist"
OPKG_ARCH=""
GOARCH=""
GOARM=""

usage() {
	cat <<EOF
Usage: build-ipk.sh -a <opkg-arch> -g <GOARCH> [-m <GOARM>] [-v <version>] [-o <output-dir>]

  -a <opkg-arch>  Required. Must exactly match \`opkg print-architecture\` on
                  the router (the non-"all" line with the highest priority).
  -g <GOARCH>     Required. The matching Go GOARCH.
  -m <GOARM>      GOARM, only meaningful when -g arm. Defaults to 7.
  -v <version>    Package version. Defaults to $VERSION.
  -o <output-dir> Where to write the .ipk. Defaults to $OUT_DIR.

Example:
  build-ipk.sh -a aarch64_cortex-a53 -g arm64 -v 1.0.0
EOF
}

while getopts "a:g:m:v:o:h" opt; do
	case "$opt" in
	a) OPKG_ARCH="$OPTARG" ;;
	g) GOARCH="$OPTARG" ;;
	m) GOARM="$OPTARG" ;;
	v) VERSION="$OPTARG" ;;
	o) OUT_DIR="$OPTARG" ;;
	h) usage; exit 0 ;;
	*) usage; exit 1 ;;
	esac
done

if [ -z "$OPKG_ARCH" ] || [ -z "$GOARCH" ]; then
	echo "error: -a <opkg-arch> and -g <GOARCH> are required" >&2
	usage
	exit 1
fi
if [ "$GOARCH" = "arm" ] && [ -z "$GOARM" ]; then
	GOARM="7"
fi

command -v ar >/dev/null || { echo "error: 'ar' not found" >&2; exit 1; }
command -v python3 >/dev/null || { echo "error: 'python3' not found (used to build root-owned tarballs)" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

STAGE="$WORK/stage"
mkdir -p "$STAGE"
cp -R "$OPENWRT_DIR/files/." "$STAGE/"

echo "==> Building $PKG_NAME for GOARCH=$GOARCH${GOARM:+ GOARM=$GOARM} (opkg arch: $OPKG_ARCH)"
mkdir -p "$STAGE/usr/bin"
(
	cd "$PROJECT_ROOT"
	env GOOS=linux GOARCH="$GOARCH" ${GOARM:+GOARM="$GOARM"} \
		go build -ldflags="-s -w" -o "$STAGE/usr/bin/$PKG_NAME" .
)

chmod 0755 "$STAGE/usr/bin/$PKG_NAME"
chmod 0755 "$STAGE/etc/init.d/$PKG_NAME"
chmod 0755 "$STAGE/usr/lib/$PKG_NAME/uci2yaml.sh"

echo "==> Assembling data.tar.gz"
DATA_TGZ="$WORK/data.tar.gz"
python3 "$SCRIPT_DIR/_mktar.py" "$STAGE" "$DATA_TGZ"

INSTALLED_SIZE="$(du -sk "$STAGE" | cut -f1)"

echo "==> Assembling control.tar.gz"
CONTROL_DIR="$WORK/control"
mkdir -p "$CONTROL_DIR"

cat > "$CONTROL_DIR/control" <<EOF
Package: $PKG_NAME
Version: $VERSION
Architecture: $OPKG_ARCH
Maintainer: derek
Section: net
Priority: optional
Depends: luci-base
Installed-Size: $INSTALLED_SIZE
Description: Forwards HTTP(S) traffic to Charles (or another proxy) resolved
 via mDNS when it's reachable, falling back to DIRECT when it's not.
 Includes a LuCI app (Services > Dynamic PAC Proxy) for configuration and
 service control.
EOF

cat > "$CONTROL_DIR/conffiles" <<EOF
/etc/config/$PKG_NAME
EOF

cat > "$CONTROL_DIR/postinst" <<'EOF'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
/etc/init.d/dynamic-pac-proxy enable
/etc/init.d/dynamic-pac-proxy start
exit 0
EOF

cat > "$CONTROL_DIR/prerm" <<'EOF'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
/etc/init.d/dynamic-pac-proxy stop
/etc/init.d/dynamic-pac-proxy disable
exit 0
EOF

chmod 0755 "$CONTROL_DIR/postinst" "$CONTROL_DIR/prerm"

CONTROL_TGZ="$WORK/control.tar.gz"
python3 "$SCRIPT_DIR/_mktar.py" "$CONTROL_DIR" "$CONTROL_TGZ"

echo "2.0" > "$WORK/debian-binary"

mkdir -p "$OUT_DIR"
IPK_PATH="$OUT_DIR/${PKG_NAME}_${VERSION}_${OPKG_ARCH}.ipk"

echo "==> Assembling $IPK_PATH"
(
	cd "$WORK"
	rm -f "$IPK_PATH"
	# -S: no symbol table. Without it, macOS's ar (oriented around building
	# static-library .a archives) silently replaces our three real members
	# with a "__.SYMDEF SORTED" entry, producing a broken, empty-looking ipk.
	ar rcS "$IPK_PATH" debian-binary control.tar.gz data.tar.gz
)

echo "==> Done: $IPK_PATH"
echo "    Install on the router with: opkg install $(basename "$IPK_PATH")"
