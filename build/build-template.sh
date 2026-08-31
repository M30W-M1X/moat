#!/usr/bin/env bash
# Build a microVM rootfs from a template directory under build/templates/.
#
# A template is a directory with a Dockerfile (or Containerfile) and a
# template.env. This script builds the image with podman, exports its
# userland, adds the guest agent at /sbin/guest-agent, and writes a raw
# ext4 file. It needs no root. There is no loop mount.
#
# The ext4 step reuses the deterministic settings from build-rootfs.sh: a
# fixed UUID, a fixed hash seed, eager table init, and a fake mkfs time,
# with every file time normalised to SOURCE_DATE_EPOCH. The image bytes
# still depend on what the Dockerfile installs. Templates are NOT pinned
# in images.lock. Two builds on one day give the same file. Builds weeks
# apart can differ, because Alpine package versions move.
#
# usage: build/build-template.sh NAME [AGENT_BIN]
#   NAME       template directory name under build/templates/
#   AGENT_BIN  static guest agent            (default: bin/guest-agent)
#
# output: build/artifacts/<NAME>-rootfs.ext4
#
# env (each overrides one pin):
#   SOURCE_DATE_EPOCH  FS_UUID  HASH_SEED
set -euo pipefail

NAME="${1:?usage: build/build-template.sh NAME [AGENT_BIN]}"
AGENT="${2:-bin/guest-agent}"

TDIR="build/templates/$NAME"
OUT="build/artifacts/${NAME}-rootfs.ext4"

[[ -d "$TDIR" ]] || { echo "ERROR: no template directory: $TDIR" >&2; exit 1; }
DOCKERFILE="$TDIR/Dockerfile"
[[ -f "$DOCKERFILE" ]] || DOCKERFILE="$TDIR/Containerfile"
[[ -f "$DOCKERFILE" ]] || { echo "ERROR: no Dockerfile or Containerfile in $TDIR" >&2; exit 1; }
[[ -f "$TDIR/template.env" ]] || { echo "ERROR: no template.env in $TDIR" >&2; exit 1; }

# Deterministic pins. Keep in sync with build/build-rootfs.sh.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1788134400}"
FS_UUID="${FS_UUID:-5a5a5a5a-0000-4000-8000-0000000000f0}"
HASH_SEED="${HASH_SEED:-9b7a6c5d-1e2f-4a3b-8c9d-0e1f2a3b4cf0}"
export SOURCE_DATE_EPOCH

# Template settings. template.env sets ROOTFS_SIZE and may override the pins.
# MKFS_OPTS passes extra flags to mkfs.ext4 (for example "-O ^has_journal" on a
# throwaway image).
ROOTFS_SIZE="256M"
MKFS_OPTS=""
# shellcheck disable=SC1091
source "$TDIR/template.env"

command -v podman    >/dev/null || { echo "ERROR: podman not found" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "ERROR: mkfs.ext4 (e2fsprogs) not found" >&2; exit 1; }
[[ -x "$AGENT" ]] || { echo "ERROR: agent binary is missing or not executable: $AGENT (run: make agent)" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"
work="$(mktemp -d)"
tag="moat-template-$NAME"
trap 'rm -rf "$work"; podman rmi -f "$tag" >/dev/null 2>&1 || true' EXIT
root="$work/root"
mkdir -p "$root"

echo "building image $tag from $DOCKERFILE ..."
podman build --pull=missing -q -t "$tag" -f "$DOCKERFILE" "$TDIR" >/dev/null

echo "exporting userland ..."
cid="$(podman create "$tag" /bin/true)"
podman export "$cid" | tar -C "$root" -xf -
podman rm -f "$cid" >/dev/null

echo "installing guest agent as /sbin/guest-agent ..."
install -Dm0755 "$AGENT" "$root/sbin/guest-agent"
mkdir -p "$root/proc" "$root/sys" "$root/dev" "$root/tmp" "$root/run" "$root/work"
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$root/etc/passwd" 2>/dev/null || true

# Record the installed Alpine packages next to the template. This file is a
# reference for review. It is not an input to the build.
if [[ -f "$root/lib/apk/db/installed" ]]; then
  awk -F: '/^P:/{p=$2} /^V:/{print p" "$2}' "$root/lib/apk/db/installed" | sort > "$TDIR/packages.manifest"
  echo "wrote $TDIR/packages.manifest ($(wc -l < "$TDIR/packages.manifest") packages)"
fi

echo "normalising timestamps to $SOURCE_DATE_EPOCH ..."
find "$root" -exec touch --no-dereference --date="@$SOURCE_DATE_EPOCH" {} +

echo "building ext4 ($ROOTFS_SIZE) ..."
rm -f "$OUT"
E2FSPROGS_FAKE_TIME="$SOURCE_DATE_EPOCH" mkfs.ext4 -q -F -L "$NAME" \
  -U "$FS_UUID" \
  -E "hash_seed=$HASH_SEED,lazy_itable_init=0,lazy_journal_init=0" \
  $MKFS_OPTS \
  -d "$root" "$OUT" "$ROOTFS_SIZE"

echo "wrote $OUT ($(du -h "$OUT" | cut -f1)) from $DOCKERFILE"
