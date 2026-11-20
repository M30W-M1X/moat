package server

import (
	"net/netip"
	"testing"

	"github.com/M30W-M1X/moat/internal/netconf"
)

func TestSandboxDestroyReleasesIPAM(t *testing.T) {
	// A /29 holds exactly one VM lease (two /30 blocks: tap + veth).
	alloc, err := netconf.NewAllocator(netip.MustParsePrefix("10.99.0.0/29"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := alloc.Allocate("sb-1")
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if _, err := alloc.Allocate("sb-2"); err == nil {
		t.Fatal("expected /29 allocator to be full")
	}

	plan := netconf.BuildPlan(lease, "none")
	sb := &Sandbox{
		ID:   "sb-1",
		net:  &plan,
		ipam: alloc,
	}
	sb.destroy()

	// After destroy, the slot must be free for a new sandbox.
	if _, err := alloc.Allocate("sb-2"); err != nil {
		t.Fatalf("allocate after destroy failed (IPAM leaked): %v", err)
	}
}
