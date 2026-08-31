#!/usr/bin/env bash
# Download an uncompressed Linux kernel (vmlinux) for Firecracker. The source is
# the public Firecracker CI S3 bucket. These kernels use the standard Firecracker
# guest config. That config sets CONFIG_VIRTIO_VSOCKETS=y and includes
# virtio-blk, virtio-mmio, ext4, and devtmpfs auto-mount. Phase 1 needs all of
# these.
#
# This script downloads one pinned kernel and checks its sha256. Bump the three
# KERNEL_* values together, then run `make images && make images-verify` and
# commit the new images.lock.
#
# usage: build/fetch-kernel.sh [OUT_PATH]
#   OUT_PATH  where to write the kernel (default: build/artifacts/vmlinux)
#
# env:
#   KERNEL_CI_SNAPSHOT     override the pinned CI snapshot dir
#   KERNEL_VERSION_EXACT   override the pinned kernel version
#   KERNEL_SHA256          override the pinned sha256
set -euo pipefail

OUT="${1:-build/artifacts/vmlinux}"
ARCH="$(uname -m)"

# Pinned kernel. Bump these three values together, then run
# `make images && make images-verify` and commit the new images.lock.
KERNEL_CI_SNAPSHOT="${KERNEL_CI_SNAPSHOT:-20260826-761f88fbb951-0}"
KERNEL_VERSION_EXACT="${KERNEL_VERSION_EXACT:-6.1.182}"
KERNEL_SHA256="${KERNEL_SHA256:-90f6c45a719c76c08f09ba5e80b4a7f1044e33152c92b9e45a22e87c0021bac5}"

BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
KEY="firecracker-ci/${KERNEL_CI_SNAPSHOT}/${ARCH}/vmlinux-${KERNEL_VERSION_EXACT}"
KERNEL_URL="${BUCKET}/${KEY}"

mkdir -p "$(dirname "$OUT")"

verify() {
  local f="$1"
  echo "${KERNEL_SHA256}  ${f}" | sha256sum -c - >/dev/null 2>&1
}

if [[ -s "$OUT" ]] && verify "$OUT"; then
  echo "kernel already present and verified: $OUT"
  exit 0
fi

echo "downloading $KERNEL_URL"
curl -fSL --progress-bar -o "${OUT}.tmp" "$KERNEL_URL"

if ! verify "${OUT}.tmp"; then
  got="$(sha256sum "${OUT}.tmp" | cut -d' ' -f1)"
  rm -f "${OUT}.tmp"
  echo "ERROR: kernel sha256 mismatch" >&2
  echo "  want ${KERNEL_SHA256}" >&2
  echo "  got  ${got}" >&2
  exit 1
fi
mv "${OUT}.tmp" "$OUT"
echo "wrote $OUT (${KERNEL_VERSION_EXACT}, snapshot ${KERNEL_CI_SNAPSHOT})"

# Also download the matching config file for reference. Ignore errors.
curl -fsSL -o "${OUT}.config" "${KERNEL_URL}.config" 2>/dev/null || true
