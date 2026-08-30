package netconf

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
)

// DefaultPool is the address pool used when the caller gives none.
const DefaultPool = "10.62.0.0/16"

// DefaultDNS is written into the guest resolv.conf and the kernel ip= arg.
var DefaultDNS = []string{"1.1.1.1", "8.8.8.8"}

// Plan is everything needed to build and tear down one microVM network. Build
// it with BuildPlan; apply it with Apply.
type Plan struct {
	VMID string

	NetnsName string // name under /run/netns
	NetnsPath string // /run/netns/<NetnsName>

	TapName  string // TAP inside the VM namespace, backs virtio-net
	GuestMAC string

	VethHost string // veth end in the parent namespace
	VethNS   string // veth end in the VM namespace

	// TAP link (guest eth0 <-> VM namespace).
	GuestIP netip.Addr
	TapGW   netip.Addr
	TapBits int

	// veth link (VM namespace <-> parent namespace).
	VethHostIP netip.Addr
	VethNSIP   netip.Addr
	VethBits   int

	// Uplink is the parent-namespace interface to masquerade on. "" means the
	// VM namespace still masquerades onto its veth, but the parent does not
	// forward further (isolation only, no internet).
	Uplink string

	// DNS servers written into the guest /etc/resolv.conf by the agent.
	DNS []string
}

// BuildPlan turns a Lease into a Plan. It derives short, stable names from the
// lease id, and places the namespace file under a writable directory.
func BuildPlan(l Lease, uplink string) Plan {
	short := shortHash(l.ID)
	name := "mvm-" + short
	return Plan{
		VMID:       l.ID,
		NetnsName:  name,
		NetnsPath:  filepath.Join(resolveNetnsDir(), name),
		TapName:    "tap0",
		GuestMAC:   guestMAC(l.ID),
		VethHost:   "mvh-" + short,
		VethNS:     "mvn-" + short,
		GuestIP:    l.GuestIP,
		TapGW:      l.TapGW,
		TapBits:    l.TapNet.Bits(),
		VethHostIP: l.VethHostIP,
		VethNSIP:   l.VethNSIP,
		VethBits:   l.VethNet.Bits(),
		Uplink:     uplink,
		DNS:        append([]string(nil), DefaultDNS...),
	}
}

// KernelIPArg returns the kernel "ip=" parameter that configures guest eth0
// with a static address and default route, with no DHCP.
//
//	ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>:<dns0>
func (p Plan) KernelIPArg() string {
	dns0 := ""
	if len(p.DNS) > 0 {
		dns0 = p.DNS[0]
	}
	return fmt.Sprintf("ip=%s::%s:%s:%s:eth0:off:%s",
		p.GuestIP, p.TapGW, prefixNetmask(p.TapBits), p.NetnsName, dns0)
}

// nftScript returns the ruleset for the VM namespace. The forward chain lets
// the guest reach the internet but drops traffic to the host, the other
// microVMs, and the cloud metadata range. The postrouting chain masquerades
// guest traffic onto the veth.
func (p Plan) nftScript() string {
	tap30 := netip.PrefixFrom(p.TapGW, p.TapBits).Masked()
	veth30 := netip.PrefixFrom(p.VethNSIP, p.VethBits).Masked()
	var b strings.Builder
	fmt.Fprintf(&b, "table inet mvm {\n")
	fmt.Fprintf(&b, "  chain forward {\n")
	fmt.Fprintf(&b, "    type filter hook forward priority 0; policy accept;\n")
	fmt.Fprintf(&b, "    ct state established,related accept\n")
	// The per-VM point-to-point links stay reachable.
	fmt.Fprintf(&b, "    ip daddr { %s, %s } accept\n", tap30, veth30)
	// Block the host, the other microVMs, loopback, and the metadata range.
	fmt.Fprintf(&b, "    ip daddr 169.254.0.0/16 drop\n")
	fmt.Fprintf(&b, "    ip daddr 127.0.0.0/8 drop\n")
	fmt.Fprintf(&b, "    ip daddr 10.0.0.0/8 drop\n")
	fmt.Fprintf(&b, "    ip daddr 172.16.0.0/12 drop\n")
	fmt.Fprintf(&b, "    ip daddr 192.168.0.0/16 drop\n")
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "  chain postrouting {\n")
	fmt.Fprintf(&b, "    type nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "    oifname \"%s\" masquerade\n", p.VethNS)
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	return b.String()
}

// prefixNetmask returns the dotted netmask for an IPv4 prefix length.
func prefixNetmask(bits int) string {
	var m uint32 = 0xffffffff << (32 - bits)
	return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
}

// shortHash returns 8 hex characters derived from s, for interface and
// namespace names (kernel IFNAMSIZ is 16).
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// guestMAC returns a locally administered unicast MAC derived from id, so a
// given VM id always gets the same address.
func guestMAC(id string) string {
	sum := sha1.Sum([]byte("mac:" + id))
	// Set the locally-administered bit and clear the multicast bit.
	b0 := (sum[0] | 0x02) &^ 0x01
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b0, sum[1], sum[2], sum[3], sum[4], sum[5])
}
