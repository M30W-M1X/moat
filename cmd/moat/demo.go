package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/config"
)

// runDemo starts a microVM and calls Agent.Info over vsock. This is the smoke
// test: it proves the VM starts and the host and guest can talk.
func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	mf := machineFlags{
		id:     fs.String("id", fmt.Sprintf("demo-%d", os.Getpid()), "machine id"),
		kernel: fs.String("kernel", "", "use this vmlinux path instead of the default"),
		rootfs: fs.String("rootfs", "", "use this rootfs.ext4 path instead of the default"),
		vcpus:  fs.Int64("vcpus", 1, "guest vCPUs"),
		mem:    fs.Int64("mem", 256, "guest memory MiB"),
		jail:   fs.Bool("jail", false, "run under jailer (needs root)"),
		keep:   fs.Bool("keep", false, "keep the VM running after the command"),
	}
	_ = fs.Parse(args)

	log := newLog()
	m, err := config.NewMachine(*mf.id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	mf.apply(&m, log)

	cleanupRootFS, err := preparePrivateRootFS(&m, *mf.keep)
	if err != nil {
		log.WithError(err).Error("prepare rootfs")
		return 1
	}
	defer cleanupRootFS()

	ctx, stop := signalContext()
	defer stop()

	inst, cli, err := bootMachine(ctx, m, log)
	if err != nil {
		log.WithError(err).Error("boot")
		return 1
	}
	defer stopMachine(inst, cli, log)

	info, err := cli.Info(ctx)
	if err != nil {
		log.WithError(err).Error("agent Info")
		return 1
	}
	log.WithFields(logrus.Fields{
		"agent_version": info.GetAgentVersion(),
		"guest_pid":     info.GetPid(),
		"kernel":        info.GetKernelRelease(),
		"uptime_s":      fmt.Sprintf("%.2f", info.GetUptimeSeconds()),
	}).Info("agent Info OK")

	if *mf.keep {
		log.Info("-keep is set. The VM stays running. Press Ctrl-C to exit. The VM keeps running after exit.")
		<-ctx.Done()
	}
	return 0
}
