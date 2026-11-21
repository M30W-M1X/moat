package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/agentclient"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/vm"
)

// runSnapshot cold-boots a microVM, runs an optional prep command, and writes a
// full snapshot. Restore it later with `moat restore -id <same id>`.
//
// With -diff-exec, it then restores that snapshot in a fresh process with
// dirty-page tracking on, runs the second command, and writes a diff snapshot
// into <dir>/diff. Restore the pair with `moat restore -diff`.
func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	mf := commonMachineFlags(fs, "snap")
	out := fs.String("out", "", "snapshot directory (default: run/<id>/snap)")
	exec := fs.String("exec", "", "shell command to run in the guest before the snapshot")
	diffExec := fs.String("diff-exec", "", "after the full snapshot, restore it, run this command, and write <dir>/diff")
	_ = fs.Parse(args)

	log := newLog()
	m, err := config.NewMachine(*mf.id)
	if err != nil {
		log.WithError(err).Error("build machine config")
		return 1
	}
	mf.apply(&m, log)
	// Copy the rootfs into m.RunDir so prep commands do not mutate the shared
	// template image. Keep the copy so later restores can open it.
	if _, err := preparePrivateRootFS(&m, true); err != nil {
		log.WithError(err).Error("prepare rootfs")
		return 1
	}
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

	if *exec != "" {
		res, err := cli.Exec(ctx, agentclient.ExecOptions{Argv: []string{"sh", "-c", *exec}}, os.Stdout, os.Stderr)
		if err != nil || res.Code != 0 {
			log.WithError(err).WithField("code", res.Code).Error("prep command failed")
			stopMachine(inst, cli, log)
			return 1
		}
	}

	t0 := time.Now()
	if err := inst.CreateSnapshot(ctx, dir); err != nil {
		log.WithError(err).Error("create snapshot")
		stopMachine(inst, cli, log)
		return 1
	}
	log.WithFields(logrus.Fields{
		"dir": dir, "elapsed": time.Since(t0).Round(time.Millisecond),
		"snapshot_bytes": fileSize(filepath.Join(dir, "snapshot.bin")),
		"mem_bytes":      fileSize(filepath.Join(dir, "mem.bin")),
	}).Info("full snapshot written")

	// The full snapshot left the guest paused. End that process now: a diff
	// snapshot must come from a fresh restore, not a resume (Gotcha G-12).
	stopMachine(inst, cli, log)

	if *diffExec == "" {
		return 0
	}
	return writeDiffSnapshot(ctx, m, dir, *diffExec, log)
}

// writeDiffSnapshot restores the full snapshot at dir with dirty tracking on,
// runs cmd, and writes a diff snapshot into dir/diff.
func writeDiffSnapshot(ctx context.Context, m config.Machine, dir, cmd string, log *logrus.Logger) int {
	inst, err := vm.RestoreForDiff(ctx, m, dir, log)
	if err != nil {
		log.WithError(err).Error("restore for diff")
		return 1
	}
	cli, err := agentclient.Dial(inst.VsockUDSPath(), config.VsockPort)
	if err != nil {
		log.WithError(err).Error("dial agent")
		stopMachine(inst, nil, log)
		return 1
	}
	rc, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := cli.WaitReady(rc); err != nil {
		cancel()
		log.WithError(err).Error("agent not ready after restore")
		stopMachine(inst, cli, log)
		return 1
	}
	if err := cli.SyncClock(rc); err != nil {
		log.WithError(err).Warn("guest clock resync failed")
	}
	cancel()

	res, err := cli.Exec(ctx, agentclient.ExecOptions{Argv: []string{"sh", "-c", cmd}}, os.Stdout, os.Stderr)
	if err != nil || res.Code != 0 {
		log.WithError(err).WithField("code", res.Code).Error("diff command failed")
		stopMachine(inst, cli, log)
		return 1
	}

	diffDir := filepath.Join(dir, "diff")
	t0 := time.Now()
	if err := inst.CreateSnapshotDiff(ctx, diffDir); err != nil {
		log.WithError(err).Error("create diff snapshot")
		stopMachine(inst, cli, log)
		return 1
	}
	log.WithFields(logrus.Fields{
		"dir": diffDir, "elapsed": time.Since(t0).Round(time.Millisecond),
		"snapshot_bytes": fileSize(filepath.Join(diffDir, "snapshot.bin")),
		"mem_apparent":   fileSize(filepath.Join(diffDir, "mem.bin")),
		"mem_disk_bytes": diskBytes(filepath.Join(diffDir, "mem.bin")),
	}).Info("diff snapshot written")
	stopMachine(inst, cli, log)
	return 0
}

// runRestore starts a microVM from a snapshot and reports the time to a ready
// agent. With a trailing command it runs that command in the restored guest.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	mf := commonMachineFlags(fs, "snap")
	snap := fs.String("snapshot", "", "snapshot directory (default: run/<id>/snap)")
	uffd := fs.Bool("uffd", false, "restore with the userfaultfd memory backend (pages stream on demand)")
	uffdBin := fs.String("uffd-bin", "", "path to moat-uffd (default: next to moat)")
	diff := fs.Bool("diff", false, "restore from a base snapshot plus <dir>/diff (rebases the diff memory first)")
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
	switch {
	case *diff:
		inst, err = vm.RestoreDiff(ctx, m, dir, filepath.Join(dir, "diff"), log)
	case *uffd:
		inst, err = vm.RestoreUFFD(ctx, m, dir, resolveUffdBin(*uffdBin), log)
	default:
		inst, err = vm.Restore(ctx, m, dir, log)
	}
	if err != nil {
		log.WithError(err).Error("restore")
		return 1
	}
	defer stopMachine(inst, nil, log)

	cli, err := agentclient.Dial(inst.VsockUDSPath(), config.VsockPort)
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
	if err := cli.SyncClock(rc); err != nil {
		log.WithError(err).Warn("guest clock resync failed")
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

// diskBytes returns the number of bytes a file actually occupies on disk. For a
// sparse file this is far below its apparent size.
func diskBytes(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return -1
	}
	return st.Blocks * 512
}

// resolveUffdBin returns explicit if set, else moat-uffd next to this binary,
// else the bare name for a PATH lookup.
func resolveUffdBin(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "moat-uffd")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}
