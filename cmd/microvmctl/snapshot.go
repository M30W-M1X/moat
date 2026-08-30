package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

// runSnapshot cold-boots a microVM, runs an optional prep command, and writes a
// full snapshot. Restore it later with `microvmctl restore -id <same id>`.
func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	mf := commonMachineFlags(fs, "snap")
	out := fs.String("out", "", "snapshot directory (default: run/<id>/snap)")
	exec := fs.String("exec", "", "shell command to run in the guest before the snapshot")
	_ = fs.Parse(args)

	log := newLog()
	m, err := config.NewMachine(*mf.id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	mf.apply(&m, log)
	dir := *out
	if dir == "" {
		dir = m.SnapshotDir()
	}

	ctx, stop := signalContext()
	defer stop()

	inst, cli, err := bootMachine(ctx, m, log)
	if err != nil {
		log.WithError(err).Error("boot")
		return 1
	}
	defer stopMachine(inst, cli, log)

	if *exec != "" {
		res, err := cli.Exec(ctx, agentclient.ExecOptions{Argv: []string{"sh", "-c", *exec}}, os.Stdout, os.Stderr)
		if err != nil || res.Code != 0 {
			log.WithError(err).WithField("code", res.Code).Error("prep command failed")
			return 1
		}
	}

	t0 := time.Now()
	if err := inst.CreateSnapshot(ctx, dir); err != nil {
		log.WithError(err).Error("create snapshot")
		return 1
	}
	log.WithFields(logrus.Fields{
		"dir": dir, "elapsed": time.Since(t0).Round(time.Millisecond),
		"snapshot_bytes": fileSize(filepath.Join(dir, "snapshot.bin")),
		"mem_bytes":      fileSize(filepath.Join(dir, "mem.bin")),
	}).Info("snapshot written")
	return 0
}

// runRestore starts a microVM from a snapshot and reports the time to a ready
// agent. With a trailing command it runs that command in the restored guest.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	mf := commonMachineFlags(fs, "snap")
	snap := fs.String("snapshot", "", "snapshot directory (default: run/<id>/snap)")
	uffd := fs.Bool("uffd", false, "restore with the userfaultfd memory backend (pages stream on demand)")
	uffdBin := fs.String("uffd-bin", "", "path to microvm-uffd (default: next to microvmctl)")
	_ = fs.Parse(args)
	argv := fs.Args()

	log := newLog()
	m, err := config.NewMachine(*mf.id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	mf.apply(&m, log)
	dir := *snap
	if dir == "" {
		dir = m.SnapshotDir()
	}

	ctx, stop := signalContext()
	defer stop()

	t0 := time.Now()
	var inst *vm.Instance
	if *uffd {
		inst, err = vm.RestoreUFFD(ctx, m, dir, resolveUffdBin(*uffdBin), log)
	} else {
		inst, err = vm.Restore(ctx, m, dir, log)
	}
	if err != nil {
		log.WithError(err).Error("restore")
		return 1
	}
	defer stopMachine(inst, nil, log)

	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		log.WithError(err).Error("dial agent")
		return 1
	}
	defer cli.Close()
	rc, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := cli.WaitReady(rc); err != nil {
		log.WithError(err).Error("agent not ready after restore")
		return 1
	}
	log.WithField("elapsed", time.Since(t0).Round(time.Millisecond)).Info("restored, agent ready")

	if len(argv) > 0 {
		res, err := cli.Exec(ctx, agentclient.ExecOptions{Argv: argv}, os.Stdout, os.Stderr)
		if err != nil {
			log.WithError(err).Error("exec")
			return 1
		}
		return res.Code
	}
	return 0
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// resolveUffdBin returns explicit if set, else microvm-uffd next to this binary,
// else the bare name for a PATH lookup.
func resolveUffdBin(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "microvm-uffd")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}
