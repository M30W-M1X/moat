//go:build integration

package pool_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/pool"
)

func TestPoolAcquireIsFast(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	base, err := config.NewMachine("it-pool-base")
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Validate(); err != nil {
		t.Skipf("images missing (run: make images): %v", err)
	}

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	ctx := context.Background()

	p, err := pool.New(ctx, pool.Options{Size: 3}, base, log)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	defer p.Close()

	var worst time.Duration
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		lease, err := p.Acquire(ctx)
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if d > worst {
			worst = d
		}
		res, err := lease.Client.Exec(ctx, agentclient.ExecOptions{Argv: []string{"echo", "ok"}}, nil, nil)
		if err != nil || res.Code != 0 {
			t.Fatalf("exec in leased VM: err=%v code=%d", err, res.Code)
		}
		lease.Release()
	}
	t.Logf("worst acquire over 10 cycles: %s", worst.Round(time.Millisecond))
	if worst > 50*time.Millisecond {
		t.Errorf("worst acquire %s exceeds the 50ms target", worst)
	}

	// The pool should refill back to its size within a few seconds.
	deadline := time.Now().Add(30 * time.Second)
	for p.Ready() < 3 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if p.Ready() < 3 {
		t.Errorf("pool did not refill: Ready=%d, want 3", p.Ready())
	}
}

func TestPoolReapsIdleLease(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	base, err := config.NewMachine("it-pool-idle")
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Validate(); err != nil {
		t.Skipf("images missing: %v", err)
	}
	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)
	ctx := context.Background()

	p, err := pool.New(ctx, pool.Options{Size: 1, IdleTimeout: 2 * time.Second}, base, log)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	defer p.Close()

	lease, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Do not Touch. The sweeper should force-release it.
	deadline := time.Now().Add(15 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		hcCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := lease.Client.Info(hcCtx)
		cancel()
		if err != nil {
			reaped = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !reaped {
		t.Error("idle lease was not reaped within 15s")
	}
	lease.Release() // no-op after reap; must not panic
}
