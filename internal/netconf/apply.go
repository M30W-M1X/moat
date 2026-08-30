package netconf

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// NetnsDir is where the per-VM namespace files are bind-mounted. It falls back
// to a temp directory when this path is not writable.
var NetnsDir = "/run/netns"

// Preflight checks that the tools are present and that this process may create
// a network namespace and bind mounts. It returns a helpful error when
// networking is not available.
func Preflight() error {
	for _, tool := range []string{"ip", "nft", "nsenter"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found on PATH: %w", tool, err)
		}
	}
	probe := filepath.Join(resolveNetnsDir(), "preflight")
	if err := createNetns(probe); err != nil {
		return fmt.Errorf("cannot create a network namespace (run microvmctl as root, "+
			"or inside 'unshare --user --map-root-user --mount --net'): %w", err)
	}
	deleteNetns(probe)
	return nil
}

// Apply builds the network described by p. On any error it tears down whatever
// it made. The caller must call Destroy on success.
func Apply(p Plan) (err error) {
	defer func() {
		if err != nil {
			Destroy(p)
		}
	}()

	if err := createNetns(p.NetnsPath); err != nil {
		return fmt.Errorf("create netns: %w", err)
	}

	// veth pair: VethHost stays here, VethNS goes into the VM namespace.
	if err := run("ip", "link", "add", p.VethHost, "type", "veth",
		"peer", "name", p.VethNS, "netns", p.NetnsPath); err != nil {
		return fmt.Errorf("create veth: %w", err)
	}

	// Parent side of the veth.
	if err := run("ip", "addr", "add", cidr(p.VethHostIP, p.VethBits), "dev", p.VethHost); err != nil {
		return fmt.Errorf("addr on %s: %w", p.VethHost, err)
	}
	if err := run("ip", "link", "set", p.VethHost, "up"); err != nil {
		return err
	}

	// Inside the VM namespace.
	nse := func(args ...string) error {
		return run(append([]string{"nsenter", "--net=" + p.NetnsPath, "--"}, args...)...)
	}
	steps := [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "addr", "add", cidr(p.VethNSIP, p.VethBits), "dev", p.VethNS},
		{"ip", "link", "set", p.VethNS, "up"},
		{"ip", "route", "add", "default", "via", p.VethHostIP.String()},
		{"ip", "tuntap", "add", p.TapName, "mode", "tap"},
		{"ip", "addr", "add", cidr(p.TapGW, p.TapBits), "dev", p.TapName},
		{"ip", "link", "set", p.TapName, "up"},
		{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := nse(s...); err != nil {
			return err
		}
	}
	if err := applyNFT(p.NetnsPath, p.nftScript()); err != nil {
		return fmt.Errorf("apply nft ruleset: %w", err)
	}

	// Optional: forward from the parent namespace out through a real uplink.
	if up := p.Uplink; up != "" && up != "none" {
		if err := run("sysctl", "-q", "-w", "net.ipv4.ip_forward=1"); err != nil {
			return fmt.Errorf("enable forwarding in parent: %w", err)
		}
		script := fmt.Sprintf("table inet %s {\n  chain postrouting {\n"+
			"    type nat hook postrouting priority 100; policy accept;\n"+
			"    ip saddr %s oifname \"%s\" masquerade\n  }\n}\n",
			p.parentTable(), netip.PrefixFrom(p.VethNSIP, p.VethBits).Masked(), up)
		if err := applyNFT("", script); err != nil {
			return fmt.Errorf("apply parent nat: %w", err)
		}
	}
	return nil
}

// Destroy removes everything Apply made. It ignores "not found" errors so it is
// safe to call more than once and after a partial Apply.
func Destroy(p Plan) {
	_ = run("nft", "delete", "table", "inet", p.parentTable())
	// Deleting VethHost removes the pair; its peer in the namespace goes too.
	_ = run("ip", "link", "del", p.VethHost)
	deleteNetns(p.NetnsPath)
}

func (p Plan) parentTable() string { return "mvm-up-" + shortHash(p.VMID) }

// createNetns makes a new network namespace and bind-mounts it at path so it
// outlives the creating thread. It needs CAP_SYS_ADMIN and CAP_NET_ADMIN.
func createNetns(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	_ = f.Close()

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		orig, err := netns.Get()
		if err != nil {
			done <- err
			return
		}
		defer orig.Close()

		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			done <- fmt.Errorf("unshare net: %w", err)
			return
		}
		self := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		mountErr := unix.Mount(self, path, "none", unix.MS_BIND, "")
		// Put this thread back before reporting, so the runtime can reuse it.
		setErr := netns.Set(orig)
		if mountErr != nil {
			done <- fmt.Errorf("bind-mount netns: %w", mountErr)
			return
		}
		done <- setErr
	}()

	if err := <-done; err != nil {
		_ = unix.Unmount(path, unix.MNT_DETACH)
		_ = os.Remove(path)
		return err
	}
	return nil
}

// deleteNetns unmounts and removes a namespace file made by createNetns.
func deleteNetns(path string) {
	_ = unix.Unmount(path, unix.MNT_DETACH)
	_ = os.Remove(path)
}

// resolveNetnsDir returns NetnsDir when it is writable, else a temp fallback.
func resolveNetnsDir() string {
	if err := os.MkdirAll(NetnsDir, 0o755); err == nil {
		if f, err := os.CreateTemp(NetnsDir, ".w"); err == nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return NetnsDir
		}
	}
	alt := filepath.Join(os.TempDir(), "microvm-netns")
	_ = os.MkdirAll(alt, 0o755)
	return alt
}

// applyNFT runs `nft -f -` with script, in the given namespace ("" = current).
func applyNFT(netnsPath, script string) error {
	var args []string
	if netnsPath != "" {
		args = append(args, "nsenter", "--net="+netnsPath, "--")
	}
	args = append(args, "nft", "-f", "-")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cidr(a netip.Addr, bits int) string { return fmt.Sprintf("%s/%d", a, bits) }

// DefaultUplink returns the interface that carries the default route, for use
// as Plan.Uplink on a real host.
func DefaultUplink() (string, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("read default route: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("no default route found")
}

// run executes a command and folds its output into any error.
func run(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
