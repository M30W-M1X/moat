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

.PHONY: all build daemon uffd agent images kernel rootfs proto tools demo run serve clean fmt vet test test-integration tidy

all: build daemon uffd agent

## build the host control CLI
build:
	$(GO) build $(GOFLAGS) -o $(BIN)/microvmctl ./cmd/microvmctl

## build the northbound API daemon
daemon:
	$(GO) build $(GOFLAGS) -o $(BIN)/microvmd ./cmd/microvmd

## build the userfaultfd page-fault handler (used by `restore -uffd`)
uffd:
	$(GO) build $(GOFLAGS) -o $(BIN)/microvm-uffd ./cmd/microvm-uffd

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

## Phase 5: run the API daemon (MICROVMD_TOKENS=id:secret make serve)
serve: daemon agent images
	MICROVMD_TOKENS="$${MICROVMD_TOKENS:-dev:devsecret}" $(BIN)/microvmd $(ARGS)

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

## boot real microVMs: agent RPCs, snapshot/restore, warm pool, reaper, HTTP API
## (needs /dev/kvm and `make images`)
test-integration: images
	$(GO) test -tags integration -v -timeout 20m \
		./internal/agentclient ./internal/vm ./internal/pool ./internal/server

## build a real per-VM network and check isolation from inside the guest.
## Runs unprivileged inside a user+net namespace.
test-net: build agent images
	unshare --user --map-root-user --mount --net --fork -- \
		$(GO) test -tags netint -v ./internal/netconf

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN) run
	@echo "kept $(ARTIFACTS). Run 'rm -rf $(ARTIFACTS)' to delete the downloaded images."
