package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/M30W-M1X/moat/internal/agentclient"
	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/netconf"
	"github.com/M30W-M1X/moat/internal/pool"
	"github.com/M30W-M1X/moat/internal/vm"
)

// Template names a kernel and rootfs a sandbox can start from, plus the
// per-sandbox resource caps and the exec defaults from an imported image.
type Template struct {
	Name   string
	Kernel string
	RootFS string
	VCPUs  int64
	MemMiB int64
	Limits cgroup.Limits
	DiskBW int64 // rootfs bytes/sec, 0 = no limit
	NetBW  int64 // network bytes/sec, 0 = no limit

	// PoolSize overrides the per-template warm pool size. 0 uses Config.PoolSize
	// for the default template and disables pooling for other templates.
	PoolSize int

	// Exec defaults. An imported template fills these from the image config.
	// A request value always wins over the template value.
	Env         map[string]string // added under the request env
	Dir         string            // working directory when the request sends none
	DefaultArgv []string          // argv when the request sends none
	Source      string            // "<repo>@sha256:...", informational
}

// Sandbox is one running microVM handed to an API caller.
type Sandbox struct {
	ID        string
	Template  string
	TokenID   string
	CreatedAt time.Time
	ExpiresAt time.Time
	Networked bool
	GuestIP   string

	tpl Template // the template this sandbox started from

	// Exactly one of lease / inst is set.
	lease *pool.Lease
	inst  *vm.Instance
	cli   *agentclient.Client
	net   *netconf.Plan
	ipam  *netconf.Allocator
}

func (s *Sandbox) client() *agentclient.Client { return s.cli }

// destroy stops the sandbox and frees its resources. It is safe to call once.
func (s *Sandbox) destroy() {
	if s.lease != nil {
		s.lease.Release()
		return
	}
	if s.cli != nil {
		s.cli.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if s.inst != nil {
		runDir := s.inst.Machine.RunDir
		_ = s.inst.Stop(ctx)
		if runDir != "" {
			_ = os.RemoveAll(runDir)
		}
	}
	if s.net != nil {
		netconf.Destroy(*s.net)
		if s.ipam != nil {
			s.ipam.Release(s.ID)
		}
	}
}

// registry tracks live sandboxes and enforces the concurrency quotas.
type registry struct {
	mu        sync.Mutex
	byID      map[string]*Sandbox
	perToken  map[string]int
	globalMax int
}

func newRegistry(globalMax int) *registry {
	return &registry{
		byID:      make(map[string]*Sandbox),
		perToken:  make(map[string]int),
		globalMax: globalMax,
	}
}

var errQuota = fmt.Errorf("sandbox quota reached")

// reserve checks the quotas and reserves a slot for tokenID. tokenMax is the
// effective per-token limit for this caller (0 = unlimited). Call release on a
// failure to create, or when the sandbox is destroyed.
func (r *registry) reserve(tokenID string, tokenMax int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.globalMax > 0 && len(r.byID) >= r.globalMax {
		return fmt.Errorf("%w: global limit %d", errQuota, r.globalMax)
	}
	if tokenMax > 0 && r.perToken[tokenID] >= tokenMax {
		return fmt.Errorf("%w: per-token limit %d", errQuota, tokenMax)
	}
	r.perToken[tokenID]++
	return nil
}

func (r *registry) release(tokenID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.perToken[tokenID] > 0 {
		r.perToken[tokenID]--
	}
}

func (r *registry) add(s *Sandbox) {
	r.mu.Lock()
	r.byID[s.ID] = s
	r.mu.Unlock()
}

func (r *registry) get(id string) (*Sandbox, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	return s, ok
}

func (r *registry) remove(id string) (*Sandbox, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if ok {
		delete(r.byID, id)
	}
	return s, ok
}

func (r *registry) list() []*Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Sandbox, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s)
	}
	return out
}

func newSandboxID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}

// coldBoot starts a sandbox that is not backed by the warm pool, optionally
// with an isolated network.
func coldBoot(ctx context.Context, s *Server, tpl Template, id string, networked bool) (*vm.Instance, *agentclient.Client, *netconf.Plan, error) {
	m, err := config.NewMachine(id)
	if err != nil {
		return nil, nil, nil, err
	}
	m.KernelImagePath = tpl.Kernel

	// A sandbox attaches its rootfs as read-write. Give each sandbox a
	// private copy of the template so two sandboxes never share one file.
	// The warm pool makes the same copy for its members.
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("run dir: %w", err)
	}
	rootfs := filepath.Join(m.RunDir, "rootfs.ext4")
	if err := vm.CopyRootFS(tpl.RootFS, rootfs); err != nil {
		return nil, nil, nil, fmt.Errorf("copy rootfs: %w", err)
	}
	m.RootFSPath = rootfs

	// Remove the run directory and the rootfs copy if the start fails. A
	// success clears this flag.
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(m.RunDir)
		}
	}()

	m.VCPUs = tpl.VCPUs
	m.MemMiB = tpl.MemMiB
	m.Limits = tpl.Limits
	m.DiskBWBytesPerSec = tpl.DiskBW
	m.NetBWBytesPerSec = tpl.NetBW

	var plan *netconf.Plan
	if networked {
		lease, err := s.ipam.Allocate(id)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("address allocation: %w", err)
		}
		p := netconf.BuildPlan(lease, s.uplink)
		p.RedirectPort = s.cfg.EgressRedirectPort
		if err := netconf.Apply(p); err != nil {
			s.ipam.Release(id)
			return nil, nil, nil, fmt.Errorf("network setup: %w", err)
		}
		plan = &p
		m.Net = plan
	}

	inst, err := vm.Boot(ctx, m, s.log)
	if err != nil {
		if plan != nil {
			netconf.Destroy(*plan)
			s.ipam.Release(id)
		}
		return nil, nil, nil, err
	}
	cli, err := agentclient.Dial(m.VsockUDSPath(), config.VsockPort)
	if err != nil {
		_ = inst.Stop(context.Background())
		if plan != nil {
			netconf.Destroy(*plan)
			s.ipam.Release(id)
		}
		return nil, nil, nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := cli.WaitReady(readyCtx); err != nil {
		cli.Close()
		_ = inst.Stop(context.Background())
		if plan != nil {
			netconf.Destroy(*plan)
			s.ipam.Release(id)
		}
		return nil, nil, nil, fmt.Errorf("agent not ready: %w", err)
	}
	started = true
	return inst, cli, plan, nil
}

// guestIP returns the address a networked sandbox's guest holds, or "".
func guestIP(p *netconf.Plan) string {
	if p == nil {
		return ""
	}
	return p.GuestIP.String()
}
