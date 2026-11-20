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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/agentclient"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/vm"
)

// Options configures a Pool.
type Options struct {
	Size int
	// LeaseTTL force-releases a lease this long after Acquire, whatever the
	// caller is doing. 0 means no limit.
	LeaseTTL time.Duration
	// IdleTimeout force-releases a lease when the caller has not called
	// Lease.Touch for this long. 0 means no limit.
	IdleTimeout time.Duration
	// SnapshotDir, when set, starts pool members from a snapshot in this
	// directory instead of a cold start, and resyncs the guest clock.
	SnapshotDir string
	// UseUFFD selects the userfaultfd memory backend when SnapshotDir is set.
	UseUFFD bool
	// UFFDBin sets the moat-uffd binary path. Empty searches PATH.
	UFFDBin string
	// ScratchMiB sets the ephemeral scratch disk size in MiB when base sets
	// ReadOnlyRoot. 0 defaults to 512 MiB.
	ScratchMiB int64
}

// Pool holds warm, running microVMs and reaps leases that outlive their limits.
type Pool struct {
	opt   Options
	base  config.Machine
	log   *logrus.Logger
	runID string

	mu     sync.Mutex
	ready  []*warm
	leases map[*Lease]struct{}
	waitCh chan *warm // hand-off to a blocked Acquire
	closed bool
	seq    atomic.Int64
	nboot  atomic.Int64 // members currently booting
	stopSw chan struct{}
}

type warm struct {
	inst *vm.Instance
	m    config.Machine
	cli  *agentclient.Client
}

// Lease is one acquired, running microVM. Call Touch to show it is still in
// use; call Release when done.
type Lease struct {
	Inst   *vm.Instance
	Client *agentclient.Client
	m      config.Machine
	pool   *Pool

	acquiredAt time.Time
	lastTouch  atomic.Int64 // unix nanoseconds
	released   atomic.Bool
}

// Touch records activity so the idle timeout does not reap this lease.
func (l *Lease) Touch() { l.lastTouch.Store(time.Now().UnixNano()) }

// New boots opts.Size microVMs from base, waits for each agent, and returns once
// every member is warm.
func New(ctx context.Context, opts Options, base config.Machine, log *logrus.Logger) (*Pool, error) {
	if opts.Size < 1 {
		return nil, fmt.Errorf("pool size must be >= 1")
	}
	p := &Pool{
		opt:    opts,
		base:   base,
		log:    log,
		runID:  fmt.Sprintf("pool-%d", os.Getpid()),
		leases: make(map[*Lease]struct{}),
		waitCh: make(chan *warm),
		stopSw: make(chan struct{}),
	}
	errs := make(chan error, opts.Size)
	for i := 0; i < opts.Size; i++ {
		go func() { errs <- p.grow(ctx) }()
	}
	for i := 0; i < opts.Size; i++ {
		if err := <-errs; err != nil {
			p.Close()
			return nil, fmt.Errorf("warm pool member: %w", err)
		}
	}
	if opts.LeaseTTL > 0 || opts.IdleTimeout > 0 {
		go p.sweep()
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

	l := &Lease{Inst: w.inst, Client: w.cli, m: w.m, pool: p, acquiredAt: time.Now()}
	l.Touch()
	p.mu.Lock()
	p.leases[l] = struct{}{}
	p.mu.Unlock()
	return l, nil
}

// sweep force-releases leases that pass their TTL or idle timeout.
func (p *Pool) sweep() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stopSw:
			return
		case now := <-t.C:
			p.mu.Lock()
			var expired []*Lease
			for l := range p.leases {
				ttlHit := p.opt.LeaseTTL > 0 && now.Sub(l.acquiredAt) > p.opt.LeaseTTL
				idleHit := p.opt.IdleTimeout > 0 &&
					now.Sub(time.Unix(0, l.lastTouch.Load())) > p.opt.IdleTimeout
				if ttlHit || idleHit {
					expired = append(expired, l)
				}
			}
			p.mu.Unlock()
			for _, l := range expired {
				p.log.WithField("id", l.m.ID).Warn("reaping expired lease")
				l.Release()
			}
		}
	}
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
// the background. Release is safe to call more than once.
func (l *Lease) Release() {
	if !l.released.CompareAndSwap(false, true) {
		return
	}
	l.pool.mu.Lock()
	delete(l.pool.leases, l)
	l.pool.mu.Unlock()

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

// Close stops every warm member and the sweeper. In-flight leases are
// unaffected.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stopSw)
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
	need := !p.closed && len(p.ready)+int(p.nboot.Load()) < p.opt.Size
	p.mu.Unlock()
	if !need {
		return
	}
	if err := p.grow(ctx); err != nil {
		p.log.WithError(err).Warn("pool refill failed")
	}
}

// grow boots one microVM, waits for its agent, and adds it to the pool (or
// hands it straight to a waiting Acquire).
func (p *Pool) grow(ctx context.Context) error {
	p.nboot.Add(1)
	defer p.nboot.Add(-1)

	m, err := p.member()
	if err != nil {
		return err
	}
	var inst *vm.Instance
	if p.opt.SnapshotDir != "" {
		if p.opt.UseUFFD {
			inst, err = vm.RestoreUFFD(ctx, m, p.opt.SnapshotDir, p.opt.UFFDBin, p.log)
		} else {
			inst, err = vm.Restore(ctx, m, p.opt.SnapshotDir, p.log)
		}
	} else {
		inst, err = vm.Boot(ctx, m, p.log)
	}
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
	if err == nil && p.opt.SnapshotDir != "" {
		if serr := cli.SyncClock(readyCtx); serr != nil {
			p.log.WithError(serr).Warn("guest clock resync failed")
		}
	}
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
	m.Limits = p.base.Limits
	m.DiskBWBytesPerSec = p.base.DiskBWBytesPerSec
	m.NetBWBytesPerSec = p.base.NetBWBytesPerSec
	m.Jail = p.base.Jail
	m.JailerUID = p.base.JailerUID
	m.JailerGID = p.base.JailerGID
	m.FirecrackerBin = p.base.FirecrackerBin
	m.JailerBin = p.base.JailerBin

	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return config.Machine{}, err
	}
	if p.base.ReadOnlyRoot {
		m.ReadOnlyRoot = true
		m.RootFSPath = p.base.RootFSPath
		scratch := filepath.Join(m.RunDir, "scratch.ext4")
		if err := vm.CreateScratchExt4(scratch, p.opt.ScratchMiB); err != nil {
			_ = os.RemoveAll(m.RunDir)
			return config.Machine{}, err
		}
		m.ScratchPath = scratch
	} else if p.opt.SnapshotDir == "" {
		rootfs := filepath.Join(m.RunDir, "rootfs.ext4")
		if err := vm.CopyRootFS(p.base.RootFSPath, rootfs); err != nil {
			_ = os.RemoveAll(m.RunDir)
			return config.Machine{}, fmt.Errorf("copy rootfs: %w", err)
		}
		m.RootFSPath = rootfs
	} else {
		m.RootFSPath = p.base.RootFSPath
	}
	return m, nil
}
