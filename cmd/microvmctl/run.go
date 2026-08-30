package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/netconf"
)

// setupNet allocates an address block, builds the network for m, and applies
// it. The returned function tears the network down.
func setupNet(m *config.Machine, pool, uplink string, log *logrus.Logger) (func(), error) {
	if err := netconf.Preflight(); err != nil {
		return nil, err
	}
	prefix, err := netip.ParsePrefix(pool)
	if err != nil {
		return nil, fmt.Errorf("bad -net-pool %q: %w", pool, err)
	}
	alloc, err := netconf.NewAllocator(prefix)
	if err != nil {
		return nil, err
	}
	lease, err := alloc.Allocate(m.ID)
	if err != nil {
		return nil, err
	}

	switch uplink {
	case "auto":
		if uplink, err = netconf.DefaultUplink(); err != nil {
			log.WithError(err).Warn("no default uplink; guest gets an isolated network with no internet")
			uplink = "none"
		}
	case "none", "":
		uplink = "none"
	}

	plan := netconf.BuildPlan(lease, uplink)
	if err := netconf.Apply(plan); err != nil {
		return nil, err
	}
	m.Net = &plan
	log.WithFields(logrus.Fields{
		"netns": plan.NetnsName, "guest_ip": plan.GuestIP, "gateway": plan.TapGW, "uplink": uplink,
	}).Info("network ready")

	return func() { netconf.Destroy(plan) }, nil
}

// repeatedFlag collects a flag that the caller may pass more than once.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// runRun starts a microVM, runs one command in it, streams the output, then
// stops the VM. The process exit code becomes the microvmctl exit code.
func runRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	mf := machineFlags{
		id:     fs.String("id", fmt.Sprintf("run-%d", os.Getpid()), "machine id"),
		kernel: fs.String("kernel", "", "use this vmlinux path instead of the default"),
		rootfs: fs.String("rootfs", "", "use this rootfs.ext4 path instead of the default"),
		vcpus:  fs.Int64("vcpus", 1, "guest vCPUs"),
		mem:    fs.Int64("mem", 256, "guest memory MiB"),
		jail:   fs.Bool("jail", false, "run under jailer (needs root)"),
		keep:   fs.Bool("keep", false, "keep the VM running after the command"),
	}
	dir := fs.String("dir", "", "working directory in the guest")
	tty := fs.Bool("tty", false, "run the command attached to a PTY")
	timeout := fs.Duration("timeout", 0, "kill the command after this long; 0 means no limit")
	netOn := fs.Bool("net", false, "give the guest an isolated network (needs root or unshare -rmn)")
	uplink := fs.String("uplink", "auto", "parent interface to NAT onto: auto, none, or a name")
	netPool := fs.String("net-pool", netconf.DefaultPool, "address pool for guest networks")
	var env, puts, gets repeatedFlag
	fs.Var(&env, "env", "set an environment variable KEY=VALUE (repeatable)")
	fs.Var(&puts, "put", "copy LOCAL:REMOTE into the guest before the command (repeatable)")
	fs.Var(&gets, "get", "copy REMOTE:LOCAL out of the guest after the command (repeatable)")
	_ = fs.Parse(args)

	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "run: no command given (use: microvmctl run [flags] -- CMD [ARG...])")
		return 2
	}

	log := newLog()
	m, err := config.NewMachine(*mf.id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	mf.apply(&m, log)

	if *netOn {
		cleanup, err := setupNet(&m, *netPool, *uplink, log)
		if err != nil {
			log.WithError(err).Error("network setup")
			return 1
		}
		defer cleanup()
	}

	ctx, stop := signalContext()
	defer stop()

	inst, cli, err := bootMachine(ctx, m, log)
	if err != nil {
		log.WithError(err).Error("boot")
		return 1
	}
	defer stopMachine(inst, cli, log)

	if err := doPuts(ctx, cli, puts, log); err != nil {
		log.WithError(err).Error("put")
		return 1
	}

	res, err := cli.Exec(ctx, agentclient.ExecOptions{
		Argv:    argv,
		Env:     parseEnv(env),
		Dir:     *dir,
		TTY:     *tty,
		Timeout: *timeout,
	}, os.Stdout, os.Stderr)
	if err != nil {
		log.WithError(err).Error("exec")
		return 1
	}
	log.WithFields(logrus.Fields{
		"pid": res.PID, "code": res.Code, "signal": res.Signal, "timed_out": res.TimedOut,
	}).Info("command finished")

	if err := doGets(ctx, cli, gets, log); err != nil {
		log.WithError(err).Error("get")
		return 1
	}

	if *mf.keep {
		log.Info("-keep is set. The VM stays running. Press Ctrl-C to exit. The VM keeps running after exit.")
		<-ctx.Done()
	}

	if res.TimedOut {
		return 124 // match GNU timeout
	}
	if res.Code < 0 {
		return 128 + res.Signal
	}
	return res.Code
}

func doPuts(ctx context.Context, cli *agentclient.Client, specs repeatedFlag, log *logrus.Logger) error {
	for _, spec := range specs {
		local, remote, ok := splitPair(spec)
		if !ok {
			return fmt.Errorf("-put %q: want LOCAL:REMOTE", spec)
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		w, err := cli.PutFile(callCtx, local, remote)
		cancel()
		if err != nil {
			return fmt.Errorf("put %s -> %s: %w", local, remote, err)
		}
		log.WithFields(logrus.Fields{"local": local, "remote": remote, "bytes": w.BytesWritten, "sha256": w.SHA256}).Info("put file")
	}
	return nil
}

func doGets(ctx context.Context, cli *agentclient.Client, specs repeatedFlag, log *logrus.Logger) error {
	for _, spec := range specs {
		remote, local, ok := splitPair(spec)
		if !ok {
			return fmt.Errorf("-get %q: want REMOTE:LOCAL", spec)
		}
		callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		r, err := cli.GetFile(callCtx, remote, local)
		cancel()
		if err != nil {
			return fmt.Errorf("get %s -> %s: %w", remote, local, err)
		}
		log.WithFields(logrus.Fields{"remote": remote, "local": local, "bytes": r.Size, "sha256": r.SHA256}).Info("get file")
	}
	return nil
}

// splitPair splits "a:b" into "a" and "b" at the first colon.
func splitPair(s string) (first, second string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// parseEnv turns KEY=VALUE strings into a map.
func parseEnv(pairs repeatedFlag) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}
