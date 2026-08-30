#!/usr/bin/env bash
# Build a small raw ext4 root filesystem for the microVM. It holds a base
# userland and the guest agent. The guest agent is installed at
# /sbin/guest-agent and runs as PID 1.
#
# This script needs no root. `podman export` writes the container tarball.
# `mkfs.ext4 -d` fills the image from a directory. There is no loop mount. This
# is the Phase 1 substitute for the Dockerfile-based template pipeline in
# CLAUDE.md.
#
# usage: build/build-rootfs.sh [OUT_PATH] [AGENT_BIN]
#   OUT_PATH   image path            (default: build/artifacts/rootfs.ext4)
#   AGENT_BIN  static guest agent    (default: bin/guest-agent)
#
# env:
#   BASE_IMAGE    OCI base image     (default: docker.io/library/alpine:3.20)
#   ROOTFS_SIZE   ext4 size          (default: 128M)
set -euo pipefail

OUT="${1:-build/artifacts/rootfs.ext4}"
AGENT="${2:-bin/guest-agent}"
BASE_IMAGE="${BASE_IMAGE:-docker.io/library/alpine:3.20}"
ROOTFS_SIZE="${ROOTFS_SIZE:-128M}"

command -v podman   >/dev/null || { echo "ERROR: podman not found" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "ERROR: mkfs.ext4 (e2fsprogs) not found" >&2; exit 1; }
[[ -x "$AGENT" ]] || { echo "ERROR: agent binary is missing or not executable: $AGENT (run: make agent)" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root"

echo "pulling $BASE_IMAGE ..."
podman pull -q "$BASE_IMAGE" >/dev/null

echo "exporting base userland ..."
cid="$(podman create "$BASE_IMAGE" /bin/true)"
podman export "$cid" | tar -C "$root" -xf -
podman rm -f "$cid" >/dev/null

echo "installing guest agent as /sbin/guest-agent ..."
install -Dm0755 "$AGENT" "$root/sbin/guest-agent"
# The Firecracker CI kernels mount devtmpfs on their own. Still, create the
# mount-point directories.
mkdir -p "$root/proc" "$root/sys" "$root/dev" "$root/tmp" "$root/run"
# This helps manual debugging with `moat demo -keep` and the serial
# console.
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$root/etc/passwd" 2>/dev/null || true

echo "building ext4 ($ROOTFS_SIZE) ..."
rm -f "$OUT"
mkfs.ext4 -q -F -L rootfs -d "$root" "$OUT" "$ROOTFS_SIZE"

echo "wrote $OUT ($(du -h "$OUT" | cut -f1)) from $BASE_IMAGE"
