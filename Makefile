# microvm_controller - Phase 1
#
# make demo    build all parts, download a kernel, start a microVM, test vsock

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

## build the guest agent that runs inside the VM (static, runs as PID 1)
agent:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='-s -w' -o $(BIN)/guest-agent ./guest/agent

images: kernel rootfs

## download a vmlinux with virtio-vsock from the Firecracker CI bucket
kernel: $(KERNEL)
$(KERNEL):
	build/fetch-kernel.sh "$(KERNEL)"

## build a small ext4 rootfs (Alpine + guest-agent) without root, with mkfs.ext4 -d
rootfs: $(ROOTFS)
$(ROOTFS): agent build/build-rootfs.sh
	build/build-rootfs.sh "$(ROOTFS)" "$(BIN)/guest-agent"

## full Phase 1 exit test: start a VM and check the vsock echo
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
	@echo "kept $(ARTIFACTS). Run 'rm -rf $(ARTIFACTS)' to delete the downloaded images."
