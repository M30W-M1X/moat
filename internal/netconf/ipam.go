// Package netconf builds and applies the per-microVM network: a dedicated
// network namespace, a TAP for Firecracker, a veth pair to the parent
// namespace, and nftables rules that let the guest reach the internet but not
// the host, the other microVMs, or the cloud metadata address.
//
// Apply and Destroy need CAP_NET_ADMIN and CAP_SYS_ADMIN. Run moat as
// root, or inside a user namespace that mapped root
// (unshare --user --map-root-user --mount --net). The IPAM and rule-building
// code in this file and plan.go needs no privilege.
package netconf

import (
	"fmt"
	"net/netip"
	"sync"
)

// blockBits is the prefix length of the address block reserved per microVM. A
// /29 holds two /30 point-to-point links: one for the guest TAP, one for the
// veth to the parent namespace.
const blockBits = 29

// Allocator hands out /29 blocks from a pool CIDR and takes them back on
// release. It is safe for concurrent use.
type Allocator struct {
	mu     sync.Mutex
	pool   netip.Prefix
	step   uint64            // addresses per block
	count  uint64            // number of blocks in the pool
	used   map[uint64]string // block index -> owner id
	byID   map[string]uint64 // owner id -> block index
	cursor uint64            // next index to try
}

// NewAllocator returns an Allocator over pool. pool must be an IPv4 prefix of
// /29 or larger.
func NewAllocator(pool netip.Prefix) (*Allocator, error) {
	pool = pool.Masked()
	if !pool.Addr().Is4() {
		return nil, fmt.Errorf("pool %s is not IPv4", pool)
	}
	if pool.Bits() > blockBits {
		return nil, fmt.Errorf("pool %s is smaller than /%d", pool, blockBits)
	}
	hostBits := 32 - pool.Bits()
	blockHostBits := 32 - blockBits
	count := uint64(1) << (hostBits - blockHostBits)
	return &Allocator{
		pool:  pool,
		step:  uint64(1) << blockHostBits,
		count: count,
		used:  make(map[uint64]string),
		byID:  make(map[string]uint64),
	}, nil
}

// Lease is one allocated /29 block and the addresses derived from it.
type Lease struct {
	ID    string
	Block netip.Prefix // the /29

	// TAP link: guest <-> VM namespace.
	TapNet  netip.Prefix // base /30
	TapGW   netip.Addr   // base+1, on tap0
	GuestIP netip.Addr   // base+2, on guest eth0

	// veth link: VM namespace <-> parent namespace.
	VethNet    netip.Prefix // base+4 /30
	VethHostIP netip.Addr   // base+5, parent side
	VethNSIP   netip.Addr   // base+6, VM-namespace side
}

// Allocate returns the block held by id, or a new one. It errors when the pool
// is full.
func (a *Allocator) Allocate(id string) (Lease, error) {
	if id == "" {
		return Lease{}, fmt.Errorf("id must not be empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if idx, ok := a.byID[id]; ok {
		return a.leaseAt(id, idx), nil
	}
	for i := uint64(0); i < a.count; i++ {
		idx := (a.cursor + i) % a.count
		if _, taken := a.used[idx]; taken {
			continue
		}
		a.used[idx] = id
		a.byID[id] = idx
		a.cursor = (idx + 1) % a.count
		return a.leaseAt(id, idx), nil
	}
	return Lease{}, fmt.Errorf("address pool %s is full (%d blocks)", a.pool, a.count)
}

// Release frees the block held by id. It is a no-op when id holds nothing.
func (a *Allocator) Release(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx, ok := a.byID[id]; ok {
		delete(a.used, idx)
		delete(a.byID, id)
	}
}

// InUse reports how many blocks are allocated.
func (a *Allocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.used)
}

func (a *Allocator) leaseAt(id string, idx uint64) Lease {
	base := addrAdd(a.pool.Addr(), idx*a.step)
	return Lease{
		ID:         id,
		Block:      netip.PrefixFrom(base, blockBits),
		TapNet:     netip.PrefixFrom(base, 30),
		TapGW:      addrAdd(base, 1),
		GuestIP:    addrAdd(base, 2),
		VethNet:    netip.PrefixFrom(addrAdd(base, 4), 30),
		VethHostIP: addrAdd(base, 5),
		VethNSIP:   addrAdd(base, 6),
	}
}

// addrAdd returns a plus n, for IPv4 only.
func addrAdd(a netip.Addr, n uint64) netip.Addr {
	b := a.As4()
	v := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	v += n
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
