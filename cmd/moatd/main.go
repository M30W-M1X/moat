// Command moatd is the northbound daemon for Moat. It serves an
// HTTP/JSON API for sandbox lifecycle, command execution, and file transfer,
// with bearer-token auth, concurrency quotas, multiple templates, a warm pool,
// Prometheus metrics at /metrics, structured request logs, and an egress
// domain-filter proxy for networked sandboxes.
//
//	moatd -addr :8080 -pool 4
//	MOAT_TOKENS="ci:s3cr3t,dev:hunter2" moatd
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/M30W-M1X/moat/internal/cgroup"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/egress"
	"github.com/M30W-M1X/moat/internal/server"
	"github.com/M30W-M1X/moat/internal/vm"
)

// version is the build version. The release build sets it with -ldflags -X.
var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address for the API")
	cfgFile := flag.String("config", "", "JSON file of templates, tokens, and per-token policy")
	tokensFlag := flag.String("tokens", "", "comma list of id:secret (else $MOAT_TOKENS)")
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
	egressTransparent := flag.String("egress-transparent", "", "listen address for the transparent egress proxy; forces guest 80/443 through it")
	auditDir := flag.String("audit-dir", "", `audit log directory ("" = default, "off" disables)`)
	imageVerify := flag.String("image-verify", "", `image check: "enforce" (default), "warn", or "off"`)
	cosignIdentity := flag.String("cosign-identity", "", "cosign certificate identity for images.lock.sig")
	cosignIssuer := flag.String("cosign-issuer", "", "cosign OIDC issuer for images.lock.sig")
	secretScan := flag.Bool("secret-scan", true, "screen files, env, and stdin for credentials on the way into a sandbox")
	secretRedact := flag.Bool("secret-redact", false, "replace a detected secret with a marker instead of a 409")
	jsonLog := flag.Bool("json-log", true, "emit structured JSON logs")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("moatd", version)
		return
	}

	log := logrus.New()
	if *jsonLog {
		log.SetFormatter(&logrus.JSONFormatter{})
	}

	// image_verify precedence: flag, then env, then config file, then default.
	imgVerify := *imageVerify
	if imgVerify == "" {
		imgVerify = os.Getenv("MOAT_IMAGE_VERIFY")
	}
	cosID := *cosignIdentity
	if cosID == "" {
		cosID = os.Getenv("MOAT_COSIGN_IDENTITY")
	}
	cosIss := *cosignIssuer
	if cosIss == "" {
		cosIss = os.Getenv("MOAT_COSIGN_ISSUER")
	}

	dm, err := config.NewMachine("default-template")
	if err != nil {
		log.WithError(err).Fatal("resolve default template paths")
	}

	cfg := server.Config{
		GlobalMax:     *globalMax,
		TokenMax:      *tokenMax,
		PoolSize:      *poolSize,
		DefaultTTL:    *defaultTTL,
		MaxTTL:        *maxTTL,
		NetPool:       *netPool,
		Uplink:        *uplink,
		SecretScanOff: !*secretScan,
		SecretRedact:  *secretRedact,
	}

	var loadedAuditFromFile *loadedAudit
	if *cfgFile != "" {
		fc, err := loadConfigFile(*cfgFile, dm.KernelImagePath, dm.RootFSPath)
		if err != nil {
			log.WithError(err).Fatal("load config file")
		}
		cfg.Templates = fc.Templates
		cfg.Tokens = fc.Tokens
		cfg.Policies = fc.Policies
		cfg.ConfigSHA256 = fc.ConfigSHA256
		loadedAuditFromFile = fc.Audit
		if imgVerify == "" {
			imgVerify = fc.ImageVerify
		}
		if cosID == "" {
			cosID = fc.CosignIdentity
		}
		if cosIss == "" {
			cosIss = fc.CosignIssuer
		}
		// A non-zero value in the file's defaults block wins over the flag.
		cfg.GlobalMax = orInt(fc.GlobalMax, cfg.GlobalMax)
		cfg.TokenMax = orInt(fc.TokenMax, cfg.TokenMax)
		cfg.PoolSize = orInt(fc.Pool, cfg.PoolSize)
		if fc.DefaultTTL > 0 {
			cfg.DefaultTTL = fc.DefaultTTL
		}
		if fc.MaxTTL > 0 {
			cfg.MaxTTL = fc.MaxTTL
		}
		cfg.NetPool = orDefault(fc.NetPool, cfg.NetPool)
		if fc.Uplink != "" {
			cfg.Uplink = fc.Uplink
		}
		log.WithFields(logrus.Fields{
			"templates": len(cfg.Templates), "tokens": len(cfg.Tokens),
			"policies": len(cfg.Policies),
		}).Info("loaded config file")
	} else {
		cfg.Tokens = parseTokens(*tokensFlag)
		if len(cfg.Tokens) == 0 {
			log.Fatal("no tokens: pass -config FILE, -tokens id:secret,..., or set $MOAT_TOKENS")
		}
		cfg.Templates = []server.Template{{
			Name:   "default",
			Kernel: orDefault(*kernel, dm.KernelImagePath),
			RootFS: orDefault(*rootfs, dm.RootFSPath),
			VCPUs:  *vcpus,
			MemMiB: *mem,
			Limits: cgroup.Limits{CPUCores: *cpuMax, MemBytes: *memMax << 20, PIDs: *pidsMax},
			DiskBW: *diskBW,
			NetBW:  *netBW,
		}}
	}

	switch imgVerify {
	case "", "enforce", "warn", "off":
	default:
		log.Fatalf("image-verify must be enforce, warn, or off, got %q", imgVerify)
	}
	cfg.ImageVerify = imgVerify
	cfg.CosignIdentity = cosID
	cfg.CosignIssuer = cosIss

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Resolve the audit directory. The config file wins first. The flag wins
	// next. "off" or an explicit empty file value disables the log. An empty
	// flag keeps the resolved value.
	auditCfg := audit.Config{
		Dir: config.AuditDir(),
		Log: log,
	}
	if fa := loadedAuditFromFile; fa != nil && fa.Set {
		if fa.Dir != "" {
			auditCfg.Dir = fa.Dir
		} else {
			auditCfg.Dir = ""
		}
		auditCfg.MaxFileMB = fa.MaxFileMB
		auditCfg.MaxTotalMB = fa.MaxTotalMB
		auditCfg.MaxAge = fa.MaxAge
		auditCfg.FsyncBatch = fa.FsyncBatch
		auditCfg.FsyncInterval = fa.FsyncInterval
	}
	switch *auditDir {
	case "off":
		auditCfg.Dir = ""
	case "":
		// Keep the resolved value.
	default:
		auditCfg.Dir = *auditDir
	}
	rec, err := audit.New(auditCfg)
	if err != nil {
		log.WithError(err).Fatal("open audit log")
	}
	defer rec.Close()
	if rec.Enabled() {
		log.WithField("dir", auditCfg.Dir).Info("audit log active")
	} else {
		log.Info("audit log disabled")
	}
	cfg.Audit = rec
	cfg.Version = version

	// A crash leaves running VMMs behind. Reconcile before the daemon serves.
	if root, err := config.RunRoot(); err == nil {
		if ids := vm.Reconcile(root, nil, log); len(ids) > 0 {
			log.WithField("ids", ids).Warn("reaped orphan microVMs at startup")
			for _, id := range ids {
				_ = rec.RecordSync(ctx, audit.Event{Type: audit.TypeOrphanReap, Sandbox: id, Attrs: map[string]any{"source": "startup"}})
			}
		}
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

	if *egressTransparent != "" {
		trAddr, err := egress.ServeTransparent(ctx, *egressTransparent, proxy)
		if err != nil {
			log.WithError(err).Fatal("start transparent egress proxy")
		}
		if _, port, err := net.SplitHostPort(trAddr); err == nil {
			cfg.EgressRedirectPort, _ = strconv.Atoi(port)
		}
		log.WithField("addr", trAddr).Info("transparent egress proxy listening")
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		log.WithError(err).Fatal("build server")
	}
	proxy.SetAudit(rec, srv.IdentifyClient)
	if err := srv.Start(ctx); err != nil {
		log.WithError(err).Fatal("start server")
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		sc, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sc)
		srv.Shutdown(sc)
		close(shutdownDone)
	}()

	log.WithField("addr", *addr).Info("moatd listening")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.WithError(err).Fatal("http server")
	}
	// Wait for srv.Shutdown to write daemon.stop and the final destroy events
	// before the deferred rec.Close runs.
	<-shutdownDone
}

func parseTokens(flagVal string) map[string]string {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("MOAT_TOKENS")
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

func orInt(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}
