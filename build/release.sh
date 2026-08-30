#!/usr/bin/env bash
# Assemble a release into dist/: the linux/amd64 binaries, a tarball, and
# checksums. `make images` first if you want the kernel and rootfs bundled too.
#
#   VERSION=v0.1.0 build/release.sh
#
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OS=linux
ARCH=amd64
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
STAGE="$DIST/moat_${VERSION}_${OS}_${ARCH}"

echo "release $VERSION  ($OS/$ARCH)"
rm -rf "$DIST"
mkdir -p "$STAGE/bin" "$STAGE/share"

export CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH
LDFLAGS="-s -w -X main.version=$VERSION"

echo "  build binaries"
go build -ldflags "$LDFLAGS" -o "$STAGE/bin/moat"      ./cmd/moat
go build -ldflags "$LDFLAGS" -o "$STAGE/bin/moatd"     ./cmd/moatd
go build -ldflags "$LDFLAGS" -o "$STAGE/bin/moat-mcp"  ./cmd/moat-mcp
go build -ldflags "$LDFLAGS" -o "$STAGE/bin/moat-uffd" ./cmd/moat-uffd
go build -ldflags "$LDFLAGS" -o "$STAGE/share/guest-agent" ./guest/agent

cp "$ROOT/SKILL.md" "$ROOT/deploy/moatd.service" "$ROOT/docs/moatd.example.json" "$STAGE/share/"

if [ -f "$ROOT/build/artifacts/vmlinux" ] && [ -f "$ROOT/build/artifacts/rootfs.ext4" ]; then
	echo "  bundle images"
	cp "$ROOT/build/artifacts/vmlinux" "$ROOT/build/artifacts/rootfs.ext4" "$STAGE/share/"
else
	echo "  images not built — the installer will fetch them separately"
fi

echo "  tar + checksums"
tar -C "$DIST" -czf "$DIST/moat_${VERSION}_${OS}_${ARCH}.tar.gz" "$(basename "$STAGE")"
( cd "$DIST" && sha256sum "moat_${VERSION}_${OS}_${ARCH}.tar.gz" > checksums.txt )

echo "done:"
ls -la "$DIST"
echo
echo "sign checksums.txt with:  cosign sign-blob --yes --output-signature dist/checksums.txt.sig dist/checksums.txt"
