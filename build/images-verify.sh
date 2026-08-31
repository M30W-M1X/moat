#!/usr/bin/env bash
# Rebuild the rootfs and the SBOM, then assert byte-for-byte reproducibility
# and agreement with images.lock and the checked-in repo-root images.lock.
#
# usage: build/images-verify.sh ARTIFACTS AGENT KVER KSNAP KSHA BASE SDE
set -euo pipefail

ART="$1"; AGENT="$2"; KVER="$3"; KSNAP="$4"; KSHA="$5"; BASE="$6"; SDE="$7"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

sha() { sha256sum "$1" | cut -d' ' -f1; }
fail() { echo "FAIL: $1" >&2; exit 1; }

echo "rebuilding rootfs ..."
build/build-rootfs.sh "$tmp/rootfs.ext4" "$AGENT" >/dev/null

[ "$(sha "$tmp/rootfs.ext4")" = "$(sha "$ART/rootfs.ext4")" ] \
  || fail "rootfs.ext4 is not reproducible on this toolchain"

echo "rebuilding sbom ..."
go run ./build/sbomgen -rootfs "$tmp/rootfs.ext4" -agent "$AGENT" \
  -kernel-version "$KVER" -kernel-snapshot "$KSNAP" -kernel-sha256 "$KSHA" \
  -base-image "$BASE" -source-date-epoch "$SDE" -out "$tmp/sbom.cdx.json" >/dev/null

[ "$(sha "$tmp/sbom.cdx.json")" = "$(sha "$ART/sbom.cdx.json")" ] \
  || fail "sbom.cdx.json is not reproducible"

# images.lock, minus the one non-reproducible field, must match the repo-root copy.
strip() { grep -v '"generated_at"' "$1"; }
diff <(strip "$ART/images.lock") <(strip images.lock) \
  || fail "build/artifacts/images.lock differs from the repo-root images.lock — commit the new one"

# The lock's own digests must match the freshly built files.
grep -q "\"$(sha "$ART/rootfs.ext4")\"" "$ART/images.lock" || fail "images.lock rootfs sha256 is stale"
grep -q "\"$(sha "$ART/vmlinux")\"" "$ART/images.lock"      || fail "images.lock kernel sha256 is stale"

echo "reproducible: OK"
