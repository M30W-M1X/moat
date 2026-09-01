# RHEL 9 userland for a Moat sandbox, from the Red Hat Universal Base Image.
#
# UBI is freely redistributable and pulls without a subscription. It is
# glibc-based; the static guest agent runs on it unchanged.
#
# Build: make template-image NAME=rhel9
#
# UBI 9.8. `dnf install` resolves against Red Hat's live repos, so the image
# is byte-stable within a day, not across weeks. This template is not pinned
# in images.lock. See build/templates/_skeleton/Dockerfile for how to refresh
# the digest.
FROM registry.access.redhat.com/ubi9/ubi@sha256:f405b019ad4f6ebf9097b636e83b363b1f11afe26557b7896fabff954b65a1b8

RUN dnf install -y --setopt=install_weak_deps=False --nodocs \
        git tar gzip which procps-ng findutils diffutils ca-certificates python3 \
 && dnf clean all \
 && rm -rf /var/cache/dnf /var/log/dnf* /var/log/hawkey.log

# Do not set ENTRYPOINT or CMD. The build adds /sbin/guest-agent and the
# kernel runs it as PID 1.
