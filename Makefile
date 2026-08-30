# microvm_controller
#
# make demo    build all parts, download a kernel, start a microVM, check the agent
# make run ARGS='-- uname -a'   start a VM and run a command in it

SHELL      := /bin/bash
GO         ?= go
BIN        := bin
ARTIFACTS  := build/artifacts
GOFLAGS    ?=
ROOTFS_SIZE ?= 256M
export GOTOOLCHAIN ?= local
export ROOTFS_SIZE

KERNEL  := $(ARTIFACTS)/vmlinux
ROOTFS  := $(ARTIFACTS)/rootfs.ext4

.PHONY: all build agent images kernel rootfs proto tools demo run clean fmt vet test test-integration tidy

all: build agent

## build the host control CLI
build:
	$(GO) build $(GOFLAGS) -o $(BIN)/microvmctl ./cmd/microvmctl

## build the guest agent that runs inside the VM (static, runs as PID 1)
agent:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='-s -w' -o $(BIN)/guest-agent ./guest/agent

## install the protobuf codegen tools into $GOBIN (no root needed)
tools:
	$(GO) install github.com/bufbuild/buf/cmd/buf@v1.47.2
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.35.2
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

## regenerate gen/ from proto/ (needs `make tools` first)
proto:
	buf lint
	buf generate

images: kernel rootfs

## download a vmlinux with virtio-vsock from the Firecracker CI bucket
kernel: $(KERNEL)
$(KERNEL):
	build/fetch-kernel.sh "$(KERNEL)"

## build a small ext4 rootfs (Alpine + guest-agent) without root, with mkfs.ext4 -d
rootfs: $(ROOTFS)
$(ROOTFS): agent build/build-rootfs.sh
	build/build-rootfs.sh "$(ROOTFS)" "$(BIN)/guest-agent"

## Phase 1 exit test: start a VM and check the agent over vsock
demo: build agent images
	$(BIN)/microvmctl demo

## Phase 2: start a VM and run a command in it, e.g. make run ARGS='-- uname -a'
run: build agent images
	$(BIN)/microvmctl run $(ARGS)

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

## boot real microVMs and drive the agent (needs /dev/kvm and `make images`)
test-integration: images
	$(GO) test -tags integration -v ./internal/agentclient

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN) run
	@echo "kept $(ARTIFACTS). Run 'rm -rf $(ARTIFACTS)' to delete the downloaded images."
