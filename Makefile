# Moat — Makefile
#
# make demo    build all parts, download a kernel, start a microVM, check the agent
# make run ARGS='-- uname -a'   start a VM and run a command in it

SHELL      := /bin/bash
GO         ?= go
BIN        := bin
ARTIFACTS  := build/artifacts
GOFLAGS    ?=
export GOTOOLCHAIN ?= local

KERNEL  := $(ARTIFACTS)/vmlinux
ROOTFS  := $(ARTIFACTS)/rootfs.ext4
SBOM    := $(ARTIFACTS)/sbom.cdx.json
LOCK    := $(ARTIFACTS)/images.lock

# Pins — keep in sync with build/fetch-kernel.sh and build/build-rootfs.sh.
KERNEL_VERSION_EXACT ?= 6.1.182
KERNEL_CI_SNAPSHOT   ?= 20260826-761f88fbb951-0
KERNEL_SHA256        ?= 90f6c45a719c76c08f09ba5e80b4a7f1044e33152c92b9e45a22e87c0021bac5
BASE_IMAGE           ?= docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
SOURCE_DATE_EPOCH    ?= 1788134400
export SOURCE_DATE_EPOCH

.PHONY: all build daemon mcp uffd agent images kernel rootfs sbom lock images-verify proto tools demo run serve dist clean fmt vet test test-integration tidy

all: build daemon mcp uffd agent

## build the host control CLI
build:
	$(GO) build $(GOFLAGS) -o $(BIN)/moat ./cmd/moat

## build the northbound API daemon
daemon:
	$(GO) build $(GOFLAGS) -o $(BIN)/moatd ./cmd/moatd

## build the MCP server for AI agents
mcp:
	$(GO) build $(GOFLAGS) -o $(BIN)/moat-mcp ./cmd/moat-mcp

## build the userfaultfd page-fault handler (used by `restore -uffd`)
uffd:
	$(GO) build $(GOFLAGS) -o $(BIN)/moat-uffd ./cmd/moat-uffd

## build the guest agent that runs inside the VM (static, runs as PID 1)
agent:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o $(BIN)/guest-agent ./guest/agent

## install the protobuf codegen tools into $GOBIN (no root needed)
tools:
	$(GO) install github.com/bufbuild/buf/cmd/buf@v1.47.2
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@v1.35.2
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

## regenerate gen/ from proto/ (needs `make tools` first)
proto:
	buf lint
	buf generate

images: kernel rootfs sbom lock

## download a vmlinux with virtio-vsock from the Firecracker CI bucket
kernel: $(KERNEL)
$(KERNEL):
	build/fetch-kernel.sh "$(KERNEL)"

## build a small ext4 rootfs (Alpine + guest-agent) without root, with mkfs.ext4 -d
rootfs: $(ROOTFS)
$(ROOTFS): agent build/build-rootfs.sh
	build/build-rootfs.sh "$(ROOTFS)" "$(BIN)/guest-agent"

## generate the CycloneDX SBOM for the built rootfs
sbom: $(SBOM)
$(SBOM): $(ROOTFS) agent
	$(GO) run ./build/sbomgen \
	  -rootfs "$(ROOTFS)" -agent "$(BIN)/guest-agent" \
	  -kernel-version "$(KERNEL_VERSION_EXACT)" -kernel-snapshot "$(KERNEL_CI_SNAPSHOT)" \
	  -kernel-sha256 "$(KERNEL_SHA256)" -base-image "$(BASE_IMAGE)" \
	  -source-date-epoch "$(SOURCE_DATE_EPOCH)" -out "$(SBOM)"

## write build/artifacts/images.lock from the built images and the SBOM
lock: $(LOCK)
$(LOCK): $(KERNEL) $(ROOTFS) $(SBOM) $(BIN)/guest-agent
	build/gen-images-lock.sh "$(ARTIFACTS)" \
	  "$(KERNEL_VERSION_EXACT)" "$(KERNEL_CI_SNAPSHOT)" \
	  "$(BASE_IMAGE)" "$(SOURCE_DATE_EPOCH)" > "$(LOCK)"

## build the rootfs twice and assert the bytes match images.lock
images-verify: images
	build/images-verify.sh "$(ARTIFACTS)" "$(BIN)/guest-agent" \
	  "$(KERNEL_VERSION_EXACT)" "$(KERNEL_CI_SNAPSHOT)" \
	  "$(KERNEL_SHA256)" "$(BASE_IMAGE)" "$(SOURCE_DATE_EPOCH)"

## Phase 1 exit test: start a VM and check the agent over vsock
demo: build agent images
	$(BIN)/moat demo

## Phase 2: start a VM and run a command in it, e.g. make run ARGS='-- uname -a'
run: build agent images
	$(BIN)/moat run $(ARGS)

## Phase 5: run the API daemon (MOAT_TOKENS=id:secret make serve)
serve: daemon agent images
	MOAT_TOKENS="$${MOAT_TOKENS:-dev:devsecret}" $(BIN)/moatd $(ARGS)

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

## assemble a release into dist/ (VERSION=v0.1.0 make dist)
dist:
	build/release.sh

clean:
	rm -rf $(BIN) run dist
	@echo "kept $(ARTIFACTS). Run 'rm -rf $(ARTIFACTS)' to delete the downloaded images."
