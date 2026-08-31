#!/usr/bin/env bash
# Write images.lock to stdout from the built artifacts.
#
# usage: build/gen-images-lock.sh ARTIFACTS_DIR KERNEL_VERSION KERNEL_SNAPSHOT BASE_IMAGE SOURCE_DATE_EPOCH
set -euo pipefail

ART="$1"; KVER="$2"; KSNAP="$3"; BASE="$4"; SDE="$5"
ARCH="$(uname -m)"
BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
KURL="${BUCKET}/firecracker-ci/${KSNAP}/${ARCH}/vmlinux-${KVER}"

sha() { sha256sum "$1" | cut -d' ' -f1; }

KSHA="$(sha "$ART/vmlinux")"
RSHA="$(sha "$ART/rootfs.ext4")"
SSHA="$(sha "$ART/sbom.cdx.json")"
ASHA="$(sha "bin/guest-agent")"
RSIZE="$(stat -c %s "$ART/rootfs.ext4")"
# `mkfs.ext4 -V` prints two lines. Take the first without a pipe, so a SIGPIPE
# under `set -o pipefail` does not abort the script.
MKFSV_ALL="$(mkfs.ext4 -V 2>&1)"
MKFSV="${MKFSV_ALL%%$'\n'*}"
GOV="$(go version)"; GOV="${GOV#go version }"; GOV="${GOV%% *}"
FS_UUID="${FS_UUID:-5a5a5a5a-0000-4000-8000-000000000001}"

cat <<JSON
{
  "schema": "moat.images.lock/v1",
  "source_date_epoch": ${SDE},
  "kernel": {
    "path": "vmlinux",
    "sha256": "${KSHA}",
    "ci_snapshot": "${KSNAP}",
    "version": "${KVER}",
    "source": "${KURL}"
  },
  "rootfs": {
    "path": "rootfs.ext4",
    "sha256": "${RSHA}",
    "size_bytes": ${RSIZE},
    "base_image": "${BASE}",
    "fs_uuid": "${FS_UUID}",
    "mkfs_version": "${MKFSV}"
  },
  "sbom": { "path": "sbom.cdx.json", "sha256": "${SSHA}" },
  "guest_agent": { "sha256": "${ASHA}", "go": "${GOV}" },
  "generated_by": "moat build pipeline",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
