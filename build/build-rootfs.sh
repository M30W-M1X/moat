#!/usr/bin/env bash
# Build a small raw ext4 root filesystem for the microVM. It holds a base
# userland and the guest agent. The guest agent is installed at
# /sbin/guest-agent and runs as PID 1.
#
# This script needs no root. `podman export` writes the container tarball.
# `mkfs.ext4 -d` fills the image from a directory. There is no loop mount.
#
# The build is byte-for-byte reproducible on one toolchain. It pins the base
# image by digest, normalises every file time to SOURCE_DATE_EPOCH, and gives
# mkfs.ext4 a fixed UUID, a fixed hash seed, eager table init, and a fake time.
# Bump BASE_IMAGE together with a fresh images.lock.
#
# usage: build/build-rootfs.sh [OUT_PATH] [AGENT_BIN]
#   OUT_PATH   image path            (default: build/artifacts/rootfs.ext4)
#   AGENT_BIN  static guest agent    (default: bin/guest-agent)
#
# env (each overrides one pin):
#   BASE_IMAGE  SOURCE_DATE_EPOCH  FS_UUID  HASH_SEED
set -euo pipefail

OUT="${1:-build/artifacts/rootfs.ext4}"
AGENT="${2:-bin/guest-agent}"

# Reproducibility pins. Bump BASE_IMAGE together with a fresh images.lock.
BASE_IMAGE="${BASE_IMAGE:-docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e}"
ROOTFS_SIZE="256M"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1788134400}"
FS_UUID="${FS_UUID:-5a5a5a5a-0000-4000-8000-000000000001}"
HASH_SEED="${HASH_SEED:-9b7a6c5d-1e2f-4a3b-8c9d-0e1f2a3b4c5d}"
export SOURCE_DATE_EPOCH

command -v podman   >/dev/null || { echo "ERROR: podman not found" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "ERROR: mkfs.ext4 (e2fsprogs) not found" >&2; exit 1; }
command -v debugfs >/dev/null || echo "WARN: debugfs not found — the SBOM step will fail" >&2
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

echo "normalising timestamps to $SOURCE_DATE_EPOCH ..."
find "$root" -exec touch --no-dereference --date="@$SOURCE_DATE_EPOCH" {} +

echo "building ext4 ($ROOTFS_SIZE) ..."
rm -f "$OUT"
E2FSPROGS_FAKE_TIME="$SOURCE_DATE_EPOCH" mkfs.ext4 -q -F -L rootfs \
  -U "$FS_UUID" \
  -E "hash_seed=$HASH_SEED,lazy_itable_init=0,lazy_journal_init=0" \
  -d "$root" "$OUT" "$ROOTFS_SIZE"

echo "wrote $OUT ($(du -h "$OUT" | cut -f1)) from $BASE_IMAGE"
