# microvm_controller - Phase 1
#
# make demo    build everything, fetch a kernel, boot a microVM, prove vsock

SHELL      := /bin/bash
GO         ?= go
BIN        := bin
ARTIFACTS  := build/artifacts
GOFLAGS    ?=
export GOTOOLCHAIN ?= local

KERNEL  := $(ARTIFACTS)/vmlinux
ROOTFS  := $(ARTIFACTS)/rootfs.ext4

.PHONY: all build agent images kernel rootfs demo clean fmt vet test tidy

all: build agent

## build the host control CLI
build:
	$(GO) build $(GOFLAGS) -o $(BIN)/microvmctl ./cmd/microvmctl

## build the in-VM guest agent (fully static, runs as PID 1)
agent:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='-s -w' -o $(BIN)/guest-agent ./guest/agent

images: kernel rootfs

## download a prebuilt vmlinux with virtio-vsock from the Firecracker CI bucket
kernel: $(KERNEL)
$(KERNEL):
	build/fetch-kernel.sh "$(KERNEL)"

## build a minimal ext4 rootfs (Alpine + guest-agent) without root, via mkfs.ext4 -d
rootfs: $(ROOTFS)
$(ROOTFS): agent build/build-rootfs.sh
	build/build-rootfs.sh "$(ROOTFS)" "$(BIN)/guest-agent"

## full Phase 1 exit check: boot + vsock round-trip
demo: build agent images
	$(BIN)/microvmctl demo

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN) run
	@echo "kept $(ARTIFACTS) (run 'rm -rf $(ARTIFACTS)' to drop downloaded images)"
