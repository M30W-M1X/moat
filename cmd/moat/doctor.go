package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/vm"
)

// runDoctor checks that the host can run Moat. It prints one line per check and
// exits non-zero when a required check fails.
func runDoctor(_ []string) int {
	var fails int
	check := func(name string, level string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark = level // WARN or FAIL
			if level == "FAIL" {
				fails++
			}
		}
		fmt.Printf("  %-5s %-18s %s\n", mark, name, detail)
	}

	fmt.Println("moat doctor")

	// KVM — required.
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		check("kvm", "FAIL", true, "/dev/kvm is readable and writable")
	} else if _, serr := os.Stat("/dev/kvm"); serr == nil {
		check("kvm", "FAIL", false, "/dev/kvm exists but this user cannot open it: "+err.Error())
	} else {
		check("kvm", "FAIL", false, "/dev/kvm not found — enable virtualization, or use a nested-virt VM")
	}

	// firecracker — required.
	if p, err := exec.LookPath("firecracker"); err == nil {
		check("firecracker", "FAIL", true, p+" ("+binVersion(p)+")")
	} else {
		check("firecracker", "FAIL", false, "not on PATH")
	}

	// jailer — only for -jail.
	if p, err := exec.LookPath("jailer"); err == nil {
		check("jailer", "WARN", true, p)
	} else {
		check("jailer", "WARN", false, "not on PATH — only needed for the jailed start (-jail)")
	}

	// cgroup v2 delegation — for the resource caps.
	if err := cgroup.Available(); err == nil {
		check("cgroup v2", "WARN", true, "delegated subtree is writable — resource caps will work")
	} else {
		check("cgroup v2", "WARN", false, "no delegated subtree — -cpu/-mem-max/-pids need root ("+err.Error()+")")
	}

	// nft — only for the network path.
	if p, err := exec.LookPath("nft"); err == nil {
		check("nftables", "WARN", true, p)
	} else {
		check("nftables", "WARN", false, "not on PATH — only needed for -net")
	}

	// userfaultfd — only for restore -uffd.
	if err := vm.UFFDAvailable(); err == nil {
		check("userfaultfd", "WARN", true, "available — restore -uffd will work")
	} else {
		check("userfaultfd", "WARN", false, "not available — only needed for restore -uffd ("+err.Error()+")")
	}

	// images — needed to start a VM.
	if m, err := config.NewMachine("doctor"); err == nil {
		if verr := m.Validate(); verr == nil {
			check("images", "WARN", true, "kernel and rootfs are present")
		} else {
			check("images", "WARN", false, "run: make images ("+verr.Error()+")")
		}
	} else {
		check("images", "WARN", false, err.Error())
	}

	fmt.Println()
	if fails > 0 {
		fmt.Printf("%d required check(s) failed.\n", fails)
		return 1
	}
	fmt.Println("ready.")
	return 0
}

// binVersion runs "<bin> --version" and returns its first line, or "unknown".
func binVersion(bin string) string {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if line == "" {
		return "unknown"
	}
	return line
}
