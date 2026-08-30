#!/usr/bin/env bash
# Fetch a prebuilt uncompressed Linux kernel (vmlinux) for Firecracker from the
# public Firecracker CI S3 bucket. These kernels use the upstream Firecracker
# guest config: CONFIG_VIRTIO_VSOCKETS=y, virtio-blk, virtio-mmio, ext4, and
# devtmpfs auto-mount - everything Phase 1 needs.
#
# usage: build/fetch-kernel.sh [OUT_PATH]
#   OUT_PATH  where to write the kernel (default: build/artifacts/vmlinux)
#
# env:
#   KERNEL_VERSION   version prefix to match, e.g. "6.1" (default: 6.1)
#   CI_SNAPSHOT      pin a CI dir, e.g. "20260826-761f88fbb951-0"
#                    (default: newest under firecracker-ci/)
set -euo pipefail

OUT="${1:-build/artifacts/vmlinux}"
ARCH="$(uname -m)"
KERNEL_VERSION="${KERNEL_VERSION:-6.1}"
BUCKET="https://s3.amazonaws.com/spec.ccfc.min"

mkdir -p "$(dirname "$OUT")"
if [[ -s "$OUT" ]]; then
  echo "kernel already present: $OUT ($(du -h "$OUT" | cut -f1))"
  exit 0
fi

# The CI bucket is laid out as firecracker-ci/<YYYYMMDD-<sha>-0>/<arch>/vmlinux-<ver>
snapshot="${CI_SNAPSHOT:-}"
if [[ -z "$snapshot" ]]; then
  echo "resolving newest Firecracker CI snapshot..."
  snapshot="$(curl -fsSL "${BUCKET}?list-type=2&prefix=firecracker-ci/&delimiter=/" \
    | grep -oE 'firecracker-ci/[0-9]{8}-[0-9a-f]+-0/' \
    | sed 's#firecracker-ci/##; s#/##' \
    | sort | tail -1)"
fi
[[ -n "$snapshot" ]] || { echo "ERROR: could not resolve a CI snapshot dir" >&2; exit 1; }

prefix="firecracker-ci/${snapshot}/${ARCH}/vmlinux-"
echo "  snapshot=$snapshot  arch=$ARCH  want=$KERNEL_VERSION.x"

key="$(curl -fsSL "${BUCKET}?list-type=2&prefix=${prefix}" \
  | grep -oE "${prefix}[0-9]+\.[0-9]+\.[0-9]+" \
  | sort -V -u \
  | grep -E "vmlinux-${KERNEL_VERSION}\." \
  | tail -1 || true)"

if [[ -z "$key" ]]; then
  echo "  no vmlinux-${KERNEL_VERSION}.x; falling back to newest available"
  key="$(curl -fsSL "${BUCKET}?list-type=2&prefix=${prefix}" \
    | grep -oE "${prefix}[0-9]+\.[0-9]+\.[0-9]+" \
    | sort -V -u | tail -1)"
fi
[[ -n "$key" ]] || { echo "ERROR: no vmlinux under ${prefix}" >&2; exit 1; }

url="${BUCKET}/${key}"
echo "downloading $url"
curl -fSL --progress-bar -o "${OUT}.tmp" "$url"
mv "${OUT}.tmp" "$OUT"
echo "wrote $OUT ($(du -h "$OUT" | cut -f1))  [${key##*/}]"

# Stash the config next to it for reference (best effort).
curl -fsSL -o "${OUT}.config" "${url}.config" 2>/dev/null || true
