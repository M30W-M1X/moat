//go:build netint

// Networking integration test. It builds a real per-VM network and, when
// /dev/kvm is present, boots a microVM in it and checks the isolation rules
// from inside the guest.
//
//	make test-net
//
// The test needs CAP_NET_ADMIN + CAP_SYS_ADMIN. The Makefile target runs it
// inside `unshare --user --map-root-user --mount --net`.
package netconf_test

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/netconf"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

func mustPlan(t *testing.T) netconf.Plan {
	t.Helper()
	a, err := netconf.NewAllocator(netip.MustParsePrefix(netconf.DefaultPool))
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Allocate("netint-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	return netconf.BuildPlan(l, "none")
}

func TestApplyBuildsIsolatedNetwork(t *testing.T) {
	if err := netconf.Preflight(); err != nil {
		t.Skipf("no network privilege: %v", err)
	}
	p := mustPlan(t)
	if err := netconf.Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Cleanup(func() { netconf.Destroy(p) })

	if _, err := os.Stat(p.NetnsPath); err != nil {
		t.Fatalf("netns file missing: %v", err)
	}

	links := nsenter(t, p.NetnsPath, "ip", "-o", "addr")
	for _, want := range []string{p.TapName, p.TapGW.String(), p.VethNS, p.VethNSIP.String()} {
		if !strings.Contains(links, want) {
			t.Errorf("ip addr in netns is missing %q\n%s", want, links)
		}
	}

	rules := nsenter(t, p.NetnsPath, "nft", "list", "ruleset")
	for _, want := range []string{"169.254.0.0/16 drop", "192.168.0.0/16 drop", "masquerade"} {
		if !strings.Contains(rules, want) {
			t.Errorf("nft ruleset is missing %q\n%s", want, rules)
		}
	}

	netconf.Destroy(p)
	if _, err := os.Stat(p.NetnsPath); !os.IsNotExist(err) {
		t.Errorf("netns file still present after Destroy: %v", err)
	}
	if err := exec.Command("ip", "link", "show", p.VethHost).Run(); err == nil {
		t.Errorf("veth %s still present after Destroy", p.VethHost)
	}
}

func TestGuestCannotReachMetadata(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	if err := netconf.Preflight(); err != nil {
		t.Skipf("no network privilege: %v", err)
	}

	m, err := config.NewMachine("netint-meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}
	p := mustPlan(t)
	if err := netconf.Apply(p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Cleanup(func() { netconf.Destroy(p) })
	m.Net = &p

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	ctx := context.Background()

	inst, err := vm.Boot(ctx, m, log)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		sc, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = inst.Stop(sc)
	})

	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	rc, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := cli.WaitReady(rc); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	ping := func(target string) int {
		var out bytes.Buffer
		res, err := cli.Exec(ctx, agentclient.ExecOptions{
			Argv:    []string{"ping", "-c", "1", "-W", "2", target},
			Timeout: 6 * time.Second,
		}, &out, &out)
		if err != nil {
			t.Fatalf("ping %s: %v", target, err)
		}
		t.Logf("ping %s -> code %d: %s", target, res.Code, strings.TrimSpace(out.String()))
		return res.Code
	}

	if ping(p.TapGW.String()) != 0 {
		t.Errorf("guest cannot reach its own gateway %s", p.TapGW)
	}
	if ping("169.254.169.254") == 0 {
		t.Error("guest reached the cloud metadata address; the isolation rule failed")
	}
}

func nsenter(t *testing.T, nsPath string, args ...string) string {
	t.Helper()
	out, err := exec.Command("nsenter", append([]string{"--net=" + nsPath, "--"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("nsenter %v: %v: %s", args, err, out)
	}
	return string(out)
}
