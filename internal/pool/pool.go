// Package pool keeps a set of microVMs booted and idle, with their agents
// connected, so a caller gets one in a few milliseconds instead of waiting for
// a cold boot. Each VM is used once: Release stops it, and the pool boots a
// replacement in the background.
//
// Members stay running rather than paused: on this Firecracker build the guest
// vsock stops answering after PauseVM/ResumeVM on the same process (see the
// project Gotchas). Snapshot restore, which resumes a fresh process, is not
// affected.
package pool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/agentclient"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

// Pool holds warm, paused microVMs.
type Pool struct {
	size  int
	base  config.Machine
	log   *logrus.Logger
	runID string

	mu     sync.Mutex
	ready  []*warm
	waitCh chan *warm // hand-off to a blocked Acquire
	closed bool
	seq    atomic.Int64
	nboot  atomic.Int64 // members currently booting
}

type warm struct {
	inst *vm.Instance
	m    config.Machine
	cli  *agentclient.Client // kept open across pause/resume
}

// Lease is one acquired, running microVM.
type Lease struct {
	Inst   *vm.Instance
	Client *agentclient.Client
	m      config.Machine
	pool   *Pool
}

// New boots size microVMs from base, waits for each agent, and returns once
// every member is warm.
func New(ctx context.Context, size int, base config.Machine, log *logrus.Logger) (*Pool, error) {
	if size < 1 {
		return nil, fmt.Errorf("pool size must be >= 1")
	}
	p := &Pool{
		size:   size,
		base:   base,
		log:    log,
		runID:  fmt.Sprintf("pool-%d", os.Getpid()),
		waitCh: make(chan *warm),
	}
	errs := make(chan error, size)
	for i := 0; i < size; i++ {
		go func() { errs <- p.grow(ctx) }()
	}
	for i := 0; i < size; i++ {
		if err := <-errs; err != nil {
			p.Close()
			return nil, fmt.Errorf("warm pool member: %w", err)
		}
	}
	return p, nil
}

// Acquire returns a running microVM. It resumes a warm member, or waits for one
// when the pool is briefly empty.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool is closed")
	}
	var w *warm
	if n := len(p.ready); n > 0 {
		w, p.ready = p.ready[n-1], p.ready[:n-1]
	}
	p.mu.Unlock()

	if w == nil {
		select {
		case w = <-p.waitCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	go p.refill(context.Background())

	// The member is already running with a live agent connection. A quick
	// health check catches a member that died while it waited.
	hcCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := w.cli.Info(hcCtx); err != nil {
		w.discard()
		return nil, fmt.Errorf("warm member is unhealthy: %w", err)
	}
	return &Lease{Inst: w.inst, Client: w.cli, m: w.m, pool: p}, nil
}

// discard stops a warm member and removes its files.
func (w *warm) discard() {
	if w.cli != nil {
		w.cli.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = w.inst.Stop(ctx)
	_ = os.RemoveAll(w.m.RunDir)
}

// Release stops the leased microVM and removes its files. The pool refills in
// the background.
func (l *Lease) Release() {
	if l.Client != nil {
		l.Client.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = l.Inst.Stop(ctx)
	_ = os.RemoveAll(l.m.RunDir)
	go l.pool.refill(context.Background())
}

// Ready reports how many warm members are waiting.
func (p *Pool) Ready() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready)
}

// Close stops every warm member. In-flight leases are unaffected.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	members := p.ready
	p.ready = nil
	p.mu.Unlock()

	for _, w := range members {
		w.discard()
	}
}

// refill boots one member when the pool is below target and not closed.
func (p *Pool) refill(ctx context.Context) {
	p.mu.Lock()
	need := !p.closed && len(p.ready)+int(p.nboot.Load()) < p.size
	p.mu.Unlock()
	if !need {
		return
	}
	if err := p.grow(ctx); err != nil {
		p.log.WithError(err).Warn("pool refill failed")
	}
}

// grow boots one microVM, waits for its agent, pauses it, and adds it to the
// pool (or hands it straight to a waiting Acquire).
func (p *Pool) grow(ctx context.Context) error {
	p.nboot.Add(1)
	defer p.nboot.Add(-1)

	m, err := p.member()
	if err != nil {
		return err
	}
	inst, err := vm.Boot(ctx, m, p.log)
	if err != nil {
		_ = os.RemoveAll(m.RunDir)
		return err
	}

	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		_ = inst.Stop(context.Background())
		_ = os.RemoveAll(m.RunDir)
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = cli.WaitReady(readyCtx)
	cancel()
	if err != nil {
		cli.Close()
		_ = inst.Stop(context.Background())
		_ = os.RemoveAll(m.RunDir)
		return fmt.Errorf("agent not ready: %w", err)
	}

	w := &warm{inst: inst, m: m, cli: cli}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		w.discard()
		return nil
	}
	p.mu.Unlock()

	select {
	case p.waitCh <- w: // a blocked Acquire took it
	default:
		p.mu.Lock()
		p.ready = append(p.ready, w)
		p.mu.Unlock()
	}
	return nil
}

// member builds a config.Machine for a new pool member with its own run
// directory and a private copy of the rootfs.
func (p *Pool) member() (config.Machine, error) {
	n := p.seq.Add(1)
	m, err := config.NewMachine(fmt.Sprintf("%s-%d", p.runID, n))
	if err != nil {
		return config.Machine{}, err
	}
	m.KernelImagePath = p.base.KernelImagePath
	m.VCPUs = p.base.VCPUs
	m.MemMiB = p.base.MemMiB

	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return config.Machine{}, err
	}
	rootfs := filepath.Join(m.RunDir, "rootfs.ext4")
	if err := copyFile(p.base.RootFSPath, rootfs); err != nil {
		return config.Machine{}, fmt.Errorf("copy rootfs: %w", err)
	}
	m.RootFSPath = rootfs
	return m, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
