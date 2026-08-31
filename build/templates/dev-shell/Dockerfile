# Broad development shell for Moat demos and manual testing.
#
# It carries Python, Node, Go, a C toolchain, and common CLI tools. Use it
# to prove the sandbox runs real work: build a project, run its tests, pull
# an artifact back out. Build with: make demo-image
#
# This image is large (~400 MiB of userland). It is a demo target, not a
# production template. Trim the package list for real workloads.
#
# Alpine 3.20 digest. See build/templates/_skeleton/Dockerfile for how to
# refresh it.
FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e

RUN apk add --no-cache \
    bash coreutils ca-certificates curl git \
    python3 py3-pip \
    nodejs npm \
    go build-base \
    ripgrep jq

# Keep the Go toolchain fixed to the installed version. Do not download one.
ENV GOTOOLCHAIN=local GOFLAGS=-mod=mod
