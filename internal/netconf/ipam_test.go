package netconf

import (
	"fmt"
	"net/netip"
	"testing"
)

func TestNewAllocatorRejectsBadPools(t *testing.T) {
	for _, s := range []string{"::1/128", "10.0.0.0/30", "192.168.1.0/31"} {
		if _, err := NewAllocator(netip.MustParsePrefix(s)); err == nil {
			t.Errorf("NewAllocator(%s) should fail", s)
		}
	}
	if _, err := NewAllocator(netip.MustParsePrefix("10.62.0.0/16")); err != nil {
		t.Errorf("NewAllocator(/16): %v", err)
	}
}

func TestAllocateAddresses(t *testing.T) {
	a, err := NewAllocator(netip.MustParsePrefix("10.62.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	l1, err := a.Allocate("vm1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Block":      "10.62.0.0/29",
		"TapNet":     "10.62.0.0/30",
		"TapGW":      "10.62.0.1",
		"GuestIP":    "10.62.0.2",
		"VethNet":    "10.62.0.4/30",
		"VethHostIP": "10.62.0.5",
		"VethNSIP":   "10.62.0.6",
	}
	got := map[string]string{
		"Block": l1.Block.String(), "TapNet": l1.TapNet.String(), "TapGW": l1.TapGW.String(),
		"GuestIP": l1.GuestIP.String(), "VethNet": l1.VethNet.String(),
		"VethHostIP": l1.VethHostIP.String(), "VethNSIP": l1.VethNSIP.String(),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s", k, got[k], v)
		}
	}
	if l1.ID != "vm1" {
		t.Errorf("ID = %q", l1.ID)
	}

	l2, _ := a.Allocate("vm2")
	if l2.Block.String() != "10.62.0.8/29" {
		t.Errorf("second block = %s, want 10.62.0.8/29", l2.Block)
	}
}

func TestAllocateIsStableAndReleasable(t *testing.T) {
	a, _ := NewAllocator(netip.MustParsePrefix("10.62.0.0/24"))
	first, _ := a.Allocate("x")
	again, _ := a.Allocate("x")
	if first.Block != again.Block {
		t.Fatalf("same id got different blocks: %s vs %s", first.Block, again.Block)
	}
	if a.InUse() != 1 {
		t.Fatalf("InUse = %d, want 1", a.InUse())
	}
	a.Release("x")
	if a.InUse() != 0 {
		t.Fatalf("InUse after release = %d, want 0", a.InUse())
	}
}

func TestAllocatorExhaustion(t *testing.T) {
	// /29 pool holds exactly one /29 block.
	a, _ := NewAllocator(netip.MustParsePrefix("10.62.0.0/29"))
	if _, err := a.Allocate("a"); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if _, err := a.Allocate("b"); err == nil {
		t.Fatal("second allocate should fail on a full pool")
	}
	a.Release("a")
	if _, err := a.Allocate("b"); err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
}

func TestAllocatorManyDistinct(t *testing.T) {
	a, _ := NewAllocator(netip.MustParsePrefix("10.62.0.0/24")) // 32 blocks
	seen := map[netip.Prefix]bool{}
	for i := 0; i < 32; i++ {
		l, err := a.Allocate(fmt.Sprintf("vm%d", i))
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if seen[l.Block] {
			t.Fatalf("duplicate block %s", l.Block)
		}
		seen[l.Block] = true
	}
	if _, err := a.Allocate("overflow"); err == nil {
		t.Fatal("33rd allocation should fail")
	}
}
