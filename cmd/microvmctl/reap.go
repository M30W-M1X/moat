package main

import (
	"flag"
	"fmt"

	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

// runReap kills Firecracker processes left behind by a crash and removes their
// run directories and networks. With no owner list, it reaps everything under
// the run directory.
func runReap(args []string) int {
	fs := flag.NewFlagSet("reap", flag.ExitOnError)
	runDir := fs.String("run-dir", "", "run directory (default: <repo>/run)")
	_ = fs.Parse(args)

	log := newLog()
	dir := *runDir
	if dir == "" {
		var err error
		if dir, err = config.RunRoot(); err != nil {
			log.WithError(err).Error("locate run directory")
			return 1
		}
	}

	ids := vm.Reconcile(dir, nil, log)
	if len(ids) == 0 {
		fmt.Println("no orphan microVMs found")
	} else {
		fmt.Printf("reaped %d: %v\n", len(ids), ids)
	}
	return 0
}
