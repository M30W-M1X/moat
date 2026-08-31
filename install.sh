#!/bin/sh
# Moat installer.  https://moat.sh
#
#   curl -fsSL https://moat.sh/install | sh
#
# Environment / flags:
#   --user                install under $HOME instead of the system paths
#   --prefix DIR           binary directory        (default /usr/local/bin, or ~/.local/bin)
#   --data DIR             image directory         (default /var/lib/moat, or ~/.local/share/moat)
#   --version VER          release tag             (default: latest)
#   --no-service           skip the systemd unit
#   --dry-run              print the plan, change nothing
#   MOAT_REPO=owner/name   GitHub repo             (default M30W-M1X/moat)
set -eu

REPO="${MOAT_REPO:-M30W-M1X/moat}"
VERSION="${MOAT_VERSION:-latest}"
USER_MODE=0
PREFIX=""
DATA=""
CONFIG_DIR=""
WANT_SERVICE=1
DRY_RUN=0

say()  { printf '%s\n' "moat-install: $*"; }
die()  { printf '%s\n' "moat-install: error: $*" >&2; exit 1; }
run()  { if [ "$DRY_RUN" = 1 ]; then printf '  + %s\n' "$*"; else eval "$*"; fi; }

while [ $# -gt 0 ]; do
	case "$1" in
		--user) USER_MODE=1 ;;
		--prefix) PREFIX="$2"; shift ;;
		--data) DATA="$2"; shift ;;
		--version) VERSION="$2"; shift ;;
		--no-service) WANT_SERVICE=0 ;;
		--dry-run) DRY_RUN=1 ;;
		-h|--help) sed -n '2,20p' "$0"; exit 0 ;;
		*) die "unknown option $1" ;;
	esac
	shift
done

# --- preflight ---------------------------------------------------------------

[ "$(uname -s)" = "Linux" ] || die "Moat runs on Linux only (host: $(uname -s)). On macOS run it inside a Linux VM."
case "$(uname -m)" in
	x86_64|amd64) ARCH=amd64 ;;
	*) die "only x86_64 is supported right now (host: $(uname -m))." ;;
esac

if [ ! -e /dev/kvm ]; then
	say "WARNING: /dev/kvm is missing. Moat needs KVM. Enable virtualization, or use a nested-virt VM."
elif [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
	say "WARNING: /dev/kvm is not readable/writable by this user. Add yourself to the 'kvm' group."
fi

DL=""
for c in curl wget; do command -v "$c" >/dev/null 2>&1 && { DL="$c"; break; }; done
[ -n "$DL" ] || die "need curl or wget."
command -v tar >/dev/null 2>&1 || die "need tar."
if command -v sha256sum >/dev/null 2>&1; then SHA="sha256sum";
elif command -v shasum >/dev/null 2>&1; then SHA="shasum -a 256";
else die "need sha256sum or shasum."; fi

if [ "$USER_MODE" = 1 ] || [ "$(id -u)" != 0 ]; then
	[ -n "$PREFIX" ]     || PREFIX="$HOME/.local/bin"
	[ -n "$DATA" ]       || DATA="${XDG_DATA_HOME:-$HOME/.local/share}/moat"
	[ -n "$CONFIG_DIR" ] || CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/moat"
	SYSTEMD_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
	SYSTEMCTL="systemctl --user"
	USER_MODE=1
else
	[ -n "$PREFIX" ]     || PREFIX="/usr/local/bin"
	[ -n "$DATA" ]       || DATA="/var/lib/moat"
	[ -n "$CONFIG_DIR" ] || CONFIG_DIR="/etc/moat"
	SYSTEMD_DIR="/etc/systemd/system"
	SYSTEMCTL="systemctl"
fi

fetch() { # fetch URL OUT
	if [ "$DL" = curl ]; then curl -fsSL "$1" -o "$2"; else wget -qO "$2" "$1"; fi
}
fetch_stdout() {
	if [ "$DL" = curl ]; then curl -fsSL "$1"; else wget -qO- "$1"; fi
}

# --- resolve version -------------------------------------------------------

if [ "$VERSION" = latest ]; then
	say "resolving the latest release of $REPO"
	VERSION="$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
		| grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//; s/".*//')"
	[ -n "$VERSION" ] || die "could not resolve the latest release. Pass --version."
fi
say "version $VERSION"

TARBALL="moat_${VERSION}_linux_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- download + verify ---------------------------------------------------

say "downloading $TARBALL"
run "fetch '$BASE/$TARBALL' '$TMP/$TARBALL'"
run "fetch '$BASE/checksums.txt' '$TMP/checksums.txt'"

if [ "$DRY_RUN" != 1 ]; then
	( cd "$TMP" && grep " $TARBALL\$" checksums.txt | $SHA -c - ) \
		|| die "checksum mismatch for $TARBALL"
	say "sha256 ok"
	if command -v cosign >/dev/null 2>&1 && fetch "$BASE/checksums.txt.sig" "$TMP/checksums.txt.sig" 2>/dev/null; then
		cosign verify-blob --signature "$TMP/checksums.txt.sig" \
			--certificate-identity-regexp ".*" --certificate-oidc-issuer-regexp ".*" \
			"$TMP/checksums.txt" && say "cosign signature ok" || die "cosign verification failed"
	else
		say "cosign not run (binary or signature absent) — verified by sha256 only"
	fi
fi

run "tar -C '$TMP' -xzf '$TMP/$TARBALL'"
STAGE="$TMP/moat_${VERSION}_linux_${ARCH}"

# --- install -----------------------------------------------------------------

say "installing binaries to $PREFIX"
run "mkdir -p '$PREFIX' '$DATA' '$CONFIG_DIR'"
for b in moat moatd moat-mcp moat-uffd; do
	run "install -m 0755 '$STAGE/bin/$b' '$PREFIX/$b'"
done
run "install -m 0755 '$STAGE/share/guest-agent' '$DATA/guest-agent'"
for f in vmlinux rootfs.ext4 images.lock sbom.cdx.json; do
	if [ -f "$STAGE/share/$f" ]; then
		run "install -m 0644 '$STAGE/share/$f' '$DATA/$f'"
	else
		say "note: $f was not in the release — build it with 'make images' or fetch it separately"
	fi
done
run "install -m 0644 '$STAGE/share/SKILL.md' '$DATA/SKILL.md'"

# --- config ---------------------------------------------------------------

CFG="$CONFIG_DIR/moatd.json"
if [ -f "$CFG" ] && [ "$DRY_RUN" != 1 ]; then
	say "keeping the existing $CFG"
elif [ "$DRY_RUN" = 1 ]; then
	say "would write a starter config to $CFG with a random token"
else
	TOKEN="$(head -c 18 /dev/urandom 2>/dev/null | od -An -tx1 | tr -d ' \n' || echo "change-me-$(date +%s)")"
	say "writing a starter config to $CFG (token: dev:$TOKEN)"
	cat > "$CFG" <<EOF
{
  "templates": [
    { "name": "default", "kernel": "$DATA/vmlinux", "rootfs": "$DATA/rootfs.ext4", "vcpus": 1, "mem_mib": 256, "cpu_max": 1, "mem_max_mib": 1024, "pids_max": 512 }
  ],
  "tokens": [
    { "id": "dev", "secret": "$TOKEN" }
  ],
  "defaults": { "global_max": 32, "token_max": 8, "default_ttl": "15m", "max_ttl": "1h", "pool": 2 }
}
EOF
fi

# --- systemd ---------------------------------------------------------------

if [ "$WANT_SERVICE" = 1 ] && command -v systemctl >/dev/null 2>&1; then
	say "installing the systemd unit to $SYSTEMD_DIR/moatd.service"
	run "mkdir -p '$SYSTEMD_DIR'"
	if [ "$DRY_RUN" != 1 ]; then
		sed -e "s#/usr/local/bin/moatd#$PREFIX/moatd#" \
			-e "s#/etc/moat/moatd.json#$CFG#" \
			-e "s#/var/lib/moat#$DATA#g" \
			"$STAGE/share/moatd.service" > "$SYSTEMD_DIR/moatd.service"
	fi
	run "$SYSTEMCTL daemon-reload"
	SERVICE_HINT="$SYSTEMCTL enable --now moatd"
else
	SERVICE_HINT="MOAT_DATA=$DATA $PREFIX/moatd -config $CFG"
fi

# --- done ---------------------------------------------------------------

cat <<EOF

moat $VERSION installed.

  1. check the host:      $PREFIX/moat doctor
  2. start the daemon:    $SERVICE_HINT
  3. run a command:       $PREFIX/moat run -- echo hello from a microVM

  MCP config for an agent (uses the token in $CFG):

    { "mcpServers": { "moat": { "command": "$PREFIX/moat-mcp",
        "env": { "MOAT_ADDR": "http://127.0.0.1:8080", "MOAT_TOKEN": "dev:<secret>" } } } }

EOF

case ":$PATH:" in
	*":$PREFIX:"*) ;;
	*) say "note: $PREFIX is not on your PATH — add it to your shell profile." ;;
esac
