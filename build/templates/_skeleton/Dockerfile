# Template skeleton for a Moat microVM rootfs.
#
# Copy this directory to build/templates/<your-name>/, edit the RUN line and
# template.env, then run: make template-image NAME=<your-name>
#
# The FROM line MUST pin a digest. A tag alone is not reproducible.
# This digest is Alpine 3.20. Find a current one with:
#   podman pull docker.io/library/alpine:3.20 && podman inspect --format '{{index .RepoDigests 0}}' alpine:3.20
FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e

# Add the tools your workload needs. Keep the list short. Every package adds
# attack surface and start time.
RUN apk add --no-cache \
    ca-certificates

# Do not set ENTRYPOINT or CMD. The build adds the guest agent at
# /sbin/guest-agent and the kernel runs it as PID 1.
