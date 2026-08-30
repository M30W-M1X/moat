// Command microvmd is the northbound daemon for microvm_controller. It serves an
// HTTP/JSON API for sandbox lifecycle, command execution, and file transfer,
// with bearer-token auth, concurrency quotas, multiple templates, a warm pool,
// Prometheus metrics at /metrics, structured request logs, and an egress
// domain-filter proxy for networked sandboxes.
//
//	microvmd -addr :8080 -pool 4
//	MICROVMD_TOKENS="ci:s3cr3t,dev:hunter2" microvmd
package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/microvm_controller/internal/cgroup"
	"github.com/M30W-M1X/microvm_controller/internal/config"
	"github.com/M30W-M1X/microvm_controller/internal/egress"
	"github.com/M30W-M1X/microvm_controller/internal/server"
	"github.com/M30W-M1X/microvm_controller/internal/vm"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address for the API")
	tokensFlag := flag.String("tokens", "", "comma list of id:secret (else $MICROVMD_TOKENS)")
	poolSize := flag.Int("pool", 0, "warm, non-networked sandboxes to keep ready")
	globalMax := flag.Int("global-max", 64, "max concurrent sandboxes (0 = unlimited)")
	tokenMax := flag.Int("token-max", 16, "max concurrent sandboxes per token (0 = unlimited)")
	defaultTTL := flag.Duration("default-ttl", 15*time.Minute, "sandbox lifetime when the caller gives none")
	maxTTL := flag.Duration("max-ttl", time.Hour, "cap on sandbox lifetime")
	kernel := flag.String("kernel", "", "default template vmlinux (default: build/artifacts/vmlinux)")
	rootfs := flag.String("rootfs", "", "default template rootfs.ext4 (default: build/artifacts/rootfs.ext4)")
	vcpus := flag.Int64("vcpus", 1, "default template vCPUs")
	mem := flag.Int64("mem", 256, "default template memory MiB")
	cpuMax := flag.Float64("cpu-max", 0, "cgroup CPU cap in cores per sandbox (0 = none)")
	memMax := flag.Int64("mem-max", 0, "cgroup memory.max per sandbox in MiB (0 = none)")
	pidsMax := flag.Int64("pids-max", 0, "cgroup pids.max per sandbox (0 = none)")
	diskBW := flag.Int64("disk-bw", 0, "rootfs bandwidth per sandbox in bytes/sec (0 = none)")
	netBW := flag.Int64("net-bw", 0, "network bandwidth per sandbox in bytes/sec (0 = none)")
	netPool := flag.String("net-pool", "", "CIDR for networked sandbox addresses")
	uplink := flag.String("uplink", "none", "parent interface for networked sandbox NAT (or none/auto)")
	egressAddr := flag.String("egress-addr", "127.0.0.1:0", "listen address for the egress proxy")
	egressAllow := flag.String("egress-allow", "", "comma list of allowed egress domains (empty = all)")
	egressDeny := flag.String("egress-deny", "", "comma list of denied egress domains")
	jsonLog := flag.Bool("json-log", true, "emit structured JSON logs")
	flag.Parse()

	log := logrus.New()
	if *jsonLog {
		log.SetFormatter(&logrus.JSONFormatter{})
	}

	tokens := parseTokens(*tokensFlag)
	if len(tokens) == 0 {
		log.Fatal("no tokens: pass -tokens id:secret,... or set $MICROVMD_TOKENS")
	}

	dm, err := config.NewMachine("default-template")
	if err != nil {
		log.WithError(err).Fatal("resolve default template paths")
	}
	tpl := server.Template{
		Name:   "default",
		Kernel: orDefault(*kernel, dm.KernelImagePath),
		RootFS: orDefault(*rootfs, dm.RootFSPath),
		VCPUs:  *vcpus,
		MemMiB: *mem,
		Limits: cgroup.Limits{CPUCores: *cpuMax, MemBytes: *memMax << 20, PIDs: *pidsMax},
		DiskBW: *diskBW,
		NetBW:  *netBW,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A crash leaves running VMMs behind; reconcile before serving.
	if root, err := config.RunRoot(); err == nil {
		if ids := vm.Reconcile(root, nil, log); len(ids) > 0 {
			log.WithField("ids", ids).Warn("reaped orphan microVMs at startup")
		}
	}

	cfg := server.Config{
		Templates:  []server.Template{tpl},
		Tokens:     tokens,
		GlobalMax:  *globalMax,
		TokenMax:   *tokenMax,
		PoolSize:   *poolSize,
		DefaultTTL: *defaultTTL,
		MaxTTL:     *maxTTL,
		NetPool:    *netPool,
		Uplink:     *uplink,
	}

	// Egress proxy for networked sandboxes.
	proxy := egress.New(egress.Policy{
		Allow: splitList(*egressAllow),
		Deny:  splitList(*egressDeny),
	}, log, nil)
	egAddr, err := egress.Serve(ctx, *egressAddr, proxy)
	if err != nil {
		log.WithError(err).Fatal("start egress proxy")
	}
	if _, port, err := net.SplitHostPort(egAddr); err == nil {
		cfg.EgressPort, _ = strconv.Atoi(port)
	}
	log.WithField("addr", egAddr).Info("egress proxy listening")

	srv, err := server.New(cfg, log)
	if err != nil {
		log.WithError(err).Fatal("build server")
	}
	if err := srv.Start(ctx); err != nil {
		log.WithError(err).Fatal("start server")
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		sc, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sc)
		srv.Shutdown(sc)
	}()

	log.WithField("addr", *addr).Info("microvmd listening")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.WithError(err).Fatal("http server")
	}
}

func parseTokens(flagVal string) map[string]string {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("MICROVMD_TOKENS")
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, secret, ok := strings.Cut(pair, ":")
		if ok && id != "" && secret != "" {
			out[id] = secret
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
