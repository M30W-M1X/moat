package netconf

import (
	"net/netip"
	"strings"
	"testing"
)

func planForTest(t *testing.T, uplink string) Plan {
	t.Helper()
	a, err := NewAllocator(netip.MustParsePrefix("10.62.0.0/16"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Allocate("demo-1")
	if err != nil {
		t.Fatal(err)
	}
	return BuildPlan(l, uplink)
}

func TestBuildPlanNamesAndMAC(t *testing.T) {
	p := planForTest(t, "none")

	if !strings.HasPrefix(p.NetnsName, "mvm-") || !strings.HasSuffix(p.NetnsPath, "/"+p.NetnsName) {
		t.Errorf("netns name/path = %q / %q", p.NetnsName, p.NetnsPath)
	}
	for _, name := range []string{p.VethHost, p.VethNS, p.NetnsName} {
		if len(name) > 15 {
			t.Errorf("interface/netns name %q is longer than 15 chars", name)
		}
	}
	if p.VethHost == p.VethNS {
		t.Error("veth ends must have different names")
	}

	// MAC: locally administered bit set, multicast bit clear, stable per id.
	var b0 int
	if _, err := fmtSscanHex(p.GuestMAC[:2], &b0); err != nil {
		t.Fatalf("bad MAC %q: %v", p.GuestMAC, err)
	}
	if b0&0x02 == 0 {
		t.Errorf("MAC %q: locally-administered bit not set", p.GuestMAC)
	}
	if b0&0x01 != 0 {
		t.Errorf("MAC %q: multicast bit set", p.GuestMAC)
	}
	if p.GuestMAC != planForTest(t, "none").GuestMAC {
		t.Error("MAC is not stable for the same id")
	}
}

func TestKernelIPArg(t *testing.T) {
	p := planForTest(t, "none")
	got := p.KernelIPArg()
	want := "ip=10.62.0.2::10.62.0.1:255.255.255.252:" + p.NetnsName + ":eth0:off:1.1.1.1"
	if got != want {
		t.Errorf("KernelIPArg() = %q, want %q", got, want)
	}
}

func TestNFTScriptBlocksTheRightTargets(t *testing.T) {
	p := planForTest(t, "none")
	s := p.nftScript()

	mustContain := []string{
		"table inet mvm",
		"ct state established,related accept",
		"ip daddr { 10.62.0.0/30, 10.62.0.4/30 } accept", // per-VM links
		"ip daddr 169.254.0.0/16 drop",                   // cloud metadata + link-local
		"ip daddr 127.0.0.0/8 drop",
		"ip daddr 10.0.0.0/8 drop",
		"ip daddr 172.16.0.0/12 drop",
		"ip daddr 192.168.0.0/16 drop",
		"type nat hook postrouting",
		"oifname \"" + p.VethNS + "\" masquerade",
	}
	for _, m := range mustContain {
		if !strings.Contains(s, m) {
			t.Errorf("nft script is missing %q\n---\n%s", m, s)
		}
	}
}

func TestNFTScriptRedirect(t *testing.T) {
	p := planForTest(t, "none")

	if s := p.nftScript(); strings.Contains(s, "chain prerouting") {
		t.Error("no redirect requested, but the script has a prerouting chain")
	}

	p.RedirectPort = 3128
	s := p.nftScript()
	want := "iifname \"" + p.TapName + "\" tcp dport { 80, 443 } dnat to " +
		p.VethHostIP.String() + ":3128"
	if !strings.Contains(s, want) {
		t.Errorf("redirect script missing %q\n---\n%s", want, s)
	}
	if !strings.Contains(s, "type nat hook prerouting priority -100") {
		t.Errorf("redirect script missing the prerouting hook\n---\n%s", s)
	}
}

func TestNFTScriptStrictEgress(t *testing.T) {
	p := planForTest(t, "none")
	p.StrictEgress = true
	s := p.nftScript()

	mustContain := []string{
		"type filter hook forward priority 0; policy drop;",
		"ip daddr { 1.1.1.1, 8.8.8.8 } udp dport 53 accept",
		"ip daddr { 1.1.1.1, 8.8.8.8 } tcp dport 53 accept",
	}
	for _, m := range mustContain {
		if !strings.Contains(s, m) {
			t.Errorf("strict egress script missing %q\n---\n%s", m, s)
		}
	}
}

func TestPrefixNetmask(t *testing.T) {
	cases := map[int]string{30: "255.255.255.252", 24: "255.255.255.0", 16: "255.255.0.0", 8: "255.0.0.0"}
	for bits, want := range cases {
		if got := prefixNetmask(bits); got != want {
			t.Errorf("prefixNetmask(%d) = %s, want %s", bits, got, want)
		}
	}
}

// fmtSscanHex parses a two-char hex string into n.
func fmtSscanHex(s string, n *int) (int, error) {
	var v int
	for _, c := range s {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int(c-'A') + 10
		default:
			return 0, &parseErr{s}
		}
	}
	*n = v
	return 1, nil
}

type parseErr struct{ s string }

func (e *parseErr) Error() string { return "not hex: " + e.s }
