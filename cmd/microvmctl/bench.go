package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/pool"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

// runBench measures how long it takes to acquire a ready microVM from a warm
// pool. The Phase 4 exit target is a p50 under 50 ms.
func runBench(args []string) int {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	size := fs.Int("pool", 4, "warm pool size")
	iters := fs.Int("iters", 20, "number of acquire/release cycles")
	mem := fs.Int64("mem", 256, "guest memory MiB")
	vcpus := fs.Int64("vcpus", 1, "guest vCPUs")
	reap := fs.Bool("reap", true, "reap orphan microVMs before starting")
	_ = fs.Parse(args)

	log := newLog()

	if *reap {
		if root, err := config.RunRoot(); err == nil {
			if ids := vm.Reconcile(root, nil, log); len(ids) > 0 {
				log.WithField("ids", ids).Info("reaped orphan microVMs")
			}
		}
	}

	base, err := config.NewMachine("bench-base")
	if err != nil {
		log.WithError(err).Error("machine config")
		return 1
	}
	base.MemMiB, base.VCPUs = *mem, *vcpus
	if err := base.Validate(); err != nil {
		log.WithError(err).Error("images missing (run: make images)")
		return 1
	}

	ctx, stop := signalContext()
	defer stop()

	warmStart := time.Now()
	p, err := pool.New(ctx, *size, base, log)
	if err != nil {
		log.WithError(err).Error("warm the pool")
		return 1
	}
	defer p.Close()
	log.WithFields(map[string]any{
		"size": *size, "elapsed": time.Since(warmStart).Round(time.Millisecond),
	}).Info("pool warm")

	acq := make([]time.Duration, 0, *iters)
	for i := 0; i < *iters; i++ {
		t0 := time.Now()
		lease, err := p.Acquire(ctx)
		d := time.Since(t0)
		if err != nil {
			log.WithError(err).Error("acquire")
			return 1
		}
		acq = append(acq, d)

		if _, err := lease.Client.Exec(ctx, agentclient.ExecOptions{Argv: []string{"true"}}, nil, nil); err != nil {
			log.WithError(err).Warn("exec in leased VM")
		}
		lease.Release()
	}

	report(acq, *iters)
	if p50(acq) < 50*time.Millisecond {
		fmt.Println("PASS: p50 acquire < 50ms")
		return 0
	}
	fmt.Println("FAIL: p50 acquire >= 50ms")
	return 1
}

func report(d []time.Duration, n int) {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	fmt.Fprintf(os.Stdout,
		"acquire over %d cycles:  min %s  p50 %s  p90 %s  p99 %s  max %s\n",
		n, sorted[0], pct(sorted, 50), pct(sorted, 90), pct(sorted, 99), sorted[len(sorted)-1])
}

func p50(d []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return pct(sorted, 50)
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (p * (len(sorted) - 1)) / 100
	return sorted[i].Round(time.Millisecond)
}
