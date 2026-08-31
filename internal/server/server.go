// Package server is the northbound HTTP API for Moat: create and
// destroy sandboxes, run commands in them, and move files, with bearer-token
// auth, per-token and global concurrency quotas, multiple templates, Prometheus
// metrics, and structured request logs.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/M30W-M1X/moat/internal/agentclient"
	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/M30W-M1X/moat/internal/config"
	"github.com/M30W-M1X/moat/internal/netconf"
	"github.com/M30W-M1X/moat/internal/pool"
	"github.com/M30W-M1X/moat/internal/secretscan"
)

// Config configures a Server.
type Config struct {
	Templates  []Template             // the first entry is the default
	Tokens     map[string]string      // token id -> secret
	Policies   map[string]TokenPolicy // token id -> extra limits; optional
	GlobalMax  int                    // 0 = unlimited
	TokenMax   int                    // 0 = unlimited
	PoolSize   int                    // warm, non-networked sandboxes; 0 = no pool
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	NetPool    string // CIDR for networked sandboxes
	Uplink     string // parent interface for networked sandboxes ("none" to skip NAT)
	EgressPort int    // if set, networked sandboxes get HTTP(S)_PROXY on this port

	// EgressRedirectPort, when > 0, adds an nftables rule to each sandbox
	// namespace that forces guest TCP port 80 and 443 through the transparent
	// egress proxy on that port. The guest cannot opt out.
	EgressRedirectPort int

	// SecretScanOff turns the ingress secret firewall off. It is on by default.
	SecretScanOff bool
	// SecretRedact makes a detected secret get replaced with a marker and let
	// through, instead of a 409. It has no effect when SecretScanOff is set.
	SecretRedact bool
	// MaxUploadBytes caps a file upload so the scanner can hold it in memory.
	// 0 selects the default (64 MiB).
	MaxUploadBytes int64

	// Audit records boundary crossings. A nil value gives a disabled recorder.
	Audit *audit.Recorder
	// Version names the build. The daemon.start audit event records it.
	Version string
	// ConfigSHA256 is the hex sha256 of the loaded moatd.json, or "" when the
	// daemon started from flags only. The daemon.start audit event records it.
	ConfigSHA256 string
}

// TokenPolicy narrows what one token may do. A zero field falls back to the
// server-wide setting. Set it per token id in Config.Policies.
type TokenPolicy struct {
	Templates     []string      // allowed template names; empty = all
	MaxConcurrent int           // per-token concurrent sandboxes; 0 = Config.TokenMax
	MaxTTL        time.Duration // cap on a sandbox TTL; 0 = Config.MaxTTL
	DenyNetwork   bool          // reject a request for a networked sandbox

	// AllowSecretOverride lets a request from this token set "allow_secrets" to
	// pass content that the secret firewall flagged. It is off by default.
	AllowSecretOverride bool

	// AuditRead widens audit visibility. "all" lets this token read every
	// token's events and run verify. Empty means own events only.
	AuditRead string
}

// templateAllowed reports whether p permits the named template.
func (p TokenPolicy) templateAllowed(name string) bool {
	if len(p.Templates) == 0 {
		return true
	}
	for _, t := range p.Templates {
		if t == name {
			return true
		}
	}
	return false
}

// auditAll reports whether p may read every token's audit events.
func (p TokenPolicy) auditAll() bool { return p.AuditRead == "all" }

// Server holds the API state.
type Server struct {
	cfg     Config
	log     *logrus.Logger
	reg     *registry
	tmpl    map[string]Template
	defTmpl string
	pool    *pool.Pool
	ipam    *netconf.Allocator
	uplink  string
	scanner *secretscan.Scanner // nil when SecretScanOff
	maxUp   int64
	m       *metrics
	promReg *prometheus.Registry
	audit   *audit.Recorder

	baseCtx     context.Context // sandbox lifetime; set by Start
	startedAt   time.Time       // set by Start, for the daemon.stop uptime
	janitorStop chan struct{}
	destroyWG   sync.WaitGroup // in-flight sb.destroy() goroutines
}

// asyncDestroy tears a sandbox down off the request path but keeps it tracked so
// Shutdown can wait for it.
func (s *Server) asyncDestroy(sb *Sandbox) {
	s.destroyWG.Add(1)
	go func() {
		defer s.destroyWG.Done()
		sb.destroy()
	}()
}

// New builds a Server. Call Start before serving Handler.
func New(cfg Config, log *logrus.Logger) (*Server, error) {
	if len(cfg.Templates) == 0 {
		return nil, errors.New("at least one template is required")
	}
	if len(cfg.Tokens) == 0 {
		return nil, errors.New("at least one auth token is required")
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 15 * time.Minute
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = time.Hour
	}
	if cfg.NetPool == "" {
		cfg.NetPool = netconf.DefaultPool
	}
	pfx, err := netip.ParsePrefix(cfg.NetPool)
	if err != nil {
		return nil, fmt.Errorf("bad net pool %q: %w", cfg.NetPool, err)
	}
	ipam, err := netconf.NewAllocator(pfx)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:         cfg,
		log:         log,
		reg:         newRegistry(cfg.GlobalMax),
		tmpl:        map[string]Template{},
		defTmpl:     cfg.Templates[0].Name,
		ipam:        ipam,
		uplink:      cfg.Uplink,
		promReg:     prometheus.NewRegistry(),
		janitorStop: make(chan struct{}),
	}
	for _, t := range cfg.Templates {
		s.tmpl[t.Name] = t
	}

	s.maxUp = cfg.MaxUploadBytes
	if s.maxUp <= 0 {
		s.maxUp = 64 << 20
	}
	if !cfg.SecretScanOff {
		sc, err := secretscan.New()
		if err != nil {
			return nil, fmt.Errorf("build secret scanner: %w", err)
		}
		s.scanner = sc
		log.WithFields(logrus.Fields{"rules": sc.RuleCount(), "skipped": sc.Skipped()}).
			Info("ingress secret firewall active")
	}

	s.m = newMetrics(s.promReg, func() float64 {
		if s.pool == nil {
			return 0
		}
		return float64(s.pool.Ready())
	})

	s.audit = cfg.Audit
	if s.audit == nil {
		rec, err := audit.New(audit.Config{Dir: "", Metrics: audit.NopMetrics(), Log: log})
		if err != nil {
			return nil, fmt.Errorf("build disabled audit recorder: %w", err)
		}
		s.audit = rec
	} else if s.audit.Enabled() {
		s.audit.SetMetrics(promAuditMetrics{s.m})
	}
	return s, nil
}

// Start warms the pool and starts the TTL janitor. ctx bounds the lifetime of
// every sandbox the server creates, so pass a context that lives as long as the
// process, not a per-request one.
func (s *Server) Start(ctx context.Context) error {
	s.baseCtx = ctx
	if s.cfg.PoolSize > 0 {
		base, err := s.tmpl[s.defTmpl].machine()
		if err != nil {
			return err
		}
		p, err := pool.New(ctx, pool.Options{Size: s.cfg.PoolSize}, base, s.log)
		if err != nil {
			return fmt.Errorf("warm pool: %w", err)
		}
		s.pool = p
	}
	s.startedAt = time.Now()
	_ = s.audit.RecordSync(ctx, audit.Event{
		Type: audit.TypeDaemonStart,
		Attrs: map[string]any{
			"version":       s.cfg.Version,
			"templates":     templateNames(s.cfg.Templates),
			"pool_size":     s.cfg.PoolSize,
			"net_pool":      s.cfg.NetPool,
			"secret_scan":   !s.cfg.SecretScanOff,
			"egress":        s.cfg.EgressPort > 0,
			"config_sha256": s.cfg.ConfigSHA256,
		},
	})
	go s.janitor()
	return nil
}

// Shutdown destroys every sandbox and stops the pool. It waits for in-flight
// teardowns so no Firecracker process outlives the process, up to the ctx
// deadline.
func (s *Server) Shutdown(ctx context.Context) {
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	up := int64(0)
	if !s.startedAt.IsZero() {
		up = int64(time.Since(s.startedAt) / time.Second)
	}
	_ = s.audit.RecordSync(sctx, audit.Event{Type: audit.TypeDaemonStop,
		Attrs: map[string]any{"reason": "signal", "uptime_s": up}})
	scancel()

	select {
	case <-s.janitorStop: // already shut down
	default:
		close(s.janitorStop)
	}
	for _, sb := range s.reg.list() {
		s.reg.remove(sb.ID)
		s.reg.release(sb.TokenID)
		// Drain the async writer first so earlier events sort before destroy.
		s.audit.Flush()
		s.recordDestroy(ctx, sb, "shutdown")
		s.asyncDestroy(sb)
	}
	done := make(chan struct{})
	go func() { s.destroyWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown timed out waiting for sandbox teardown")
	}
	if s.pool != nil {
		s.pool.Close()
	}
}

// IdentifyClient maps an egress proxy client IP to the sandbox that owns it.
// The proxy client is the guest, so the IP is a guest address. It returns the
// sandbox id, the token id, and true on a match, or empty strings and false.
func (s *Server) IdentifyClient(ip netip.Addr) (string, string, bool) {
	for _, sb := range s.reg.list() {
		if sb.net != nil && sb.net.GuestIP.Compare(ip) == 0 {
			return sb.ID, sb.TokenID, true
		}
	}
	return "", "", false
}

// machine builds a config.Machine the warm pool clones members from.
func (t Template) machine() (config.Machine, error) {
	m, err := config.NewMachine("tmpl-" + t.Name)
	if err != nil {
		return config.Machine{}, err
	}
	m.KernelImagePath = t.Kernel
	m.RootFSPath = t.RootFS
	m.VCPUs = t.VCPUs
	m.MemMiB = t.MemMiB
	m.Limits = t.Limits
	m.DiskBWBytesPerSec = t.DiskBW
	m.NetBWBytesPerSec = t.NetBW
	return m, nil
}

// Handler returns the HTTP handler. Middleware order: recover, log+metrics, auth.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}))
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.promReg, promhttp.HandlerOpts{}))

	mux.HandleFunc("POST /v1/sandboxes", s.auth(s.handleCreate))
	mux.HandleFunc("GET /v1/sandboxes", s.auth(s.handleList))
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.auth(s.handleGet))
	mux.HandleFunc("GET /v1/sandboxes/{id}/report", s.auth(s.handleReport))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.auth(s.handleDelete))
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.auth(s.handleExec))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files/{path...}", s.auth(s.handlePutFile))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/{path...}", s.auth(s.handleGetFile))
	mux.HandleFunc("GET /v1/audit", s.auth(s.handleAudit))

	return s.recover(s.observe(mux))
}

// --- middleware ---

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.WithField("panic", v).Error("handler panic")
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wrote {
		sr.status, sr.wrote = code, true
		sr.ResponseWriter.WriteHeader(code)
	}
}
func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wrote {
		sr.WriteHeader(http.StatusOK)
	}
	return sr.ResponseWriter.Write(b)
}
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sr, r)
		route := routeLabel(r)
		dur := time.Since(start)
		s.m.httpRequests.WithLabelValues(route, statusClass(sr.status)).Inc()
		s.m.httpSeconds.WithLabelValues(route).Observe(dur.Seconds())
		s.log.WithFields(logrus.Fields{
			"method": r.Method, "route": route, "status": sr.status,
			"dur_ms": dur.Milliseconds(), "remote": r.RemoteAddr,
		}).Info("request")
	})
}

type ctxKey int

const tokenIDKey ctxKey = 0

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var id string
		for tid, secret := range s.cfg.Tokens {
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1 {
				id = tid
				break
			}
		}
		if id == "" {
			writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), tokenIDKey, id)))
	}
}

func tokenID(r *http.Request) string {
	id, _ := r.Context().Value(tokenIDKey).(string)
	return id
}

// --- handlers ---

type createReq struct {
	Template   string `json:"template"`
	Networked  bool   `json:"networked"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type sandboxView struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Networked bool      `json:"networked"`
	GuestIP   string    `json:"guest_ip,omitempty"`
}

func view(s *Sandbox) sandboxView {
	return sandboxView{s.ID, s.Template, s.CreatedAt, s.ExpiresAt, s.Networked, s.GuestIP}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
			return
		}
	}
	tid := tokenID(r)
	policy, hasPolicy := s.cfg.Policies[tid]

	name := req.Template
	if name == "" {
		name = s.defTmpl
	}
	tpl, ok := s.tmpl[name]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown template "+name)
		return
	}
	if hasPolicy && !policy.templateAllowed(name) {
		writeErr(w, http.StatusForbidden, "this token may not use template "+name)
		return
	}
	if req.Networked && hasPolicy && policy.DenyNetwork {
		writeErr(w, http.StatusForbidden, "this token may not create a networked sandbox")
		return
	}

	ttl := s.cfg.DefaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	if hasPolicy && policy.MaxTTL > 0 && ttl > policy.MaxTTL {
		ttl = policy.MaxTTL
	}

	tokenMax := s.cfg.TokenMax
	if hasPolicy && policy.MaxConcurrent > 0 {
		tokenMax = policy.MaxConcurrent
	}
	if err := s.reg.reserve(tid, tokenMax); err != nil {
		s.m.quotaRejects.Inc()
		scope := "token"
		if strings.Contains(err.Error(), "global") {
			scope = "global"
		}
		_ = s.audit.RecordSync(r.Context(), audit.Event{
			Type: audit.TypeQuotaReject, Token: tid,
			Attrs: map[string]any{"scope": scope, "template": name},
		})
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}

	id := newSandboxID()
	start := time.Now()
	sb := &Sandbox{ID: id, Template: name, TokenID: tid, CreatedAt: start,
		ExpiresAt: start.Add(ttl), Networked: req.Networked}

	source := "cold"
	if !req.Networked && s.pool != nil {
		acqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		lease, err := s.pool.Acquire(acqCtx)
		cancel()
		if err == nil {
			sb.lease, sb.cli = lease, lease.Client
			source = "pool"
		}
	}
	if sb.cli == nil {
		inst, cli, plan, err := coldBoot(s.baseCtx, s, tpl, id, req.Networked)
		if err != nil {
			s.reg.release(tid)
			writeErr(w, http.StatusInternalServerError, "start sandbox: "+err.Error())
			return
		}
		sb.inst, sb.cli, sb.net = inst, cli, plan
		sb.GuestIP = guestIP(plan)
	}

	s.reg.add(sb)
	s.m.sandboxesActive.WithLabelValues(name).Inc()
	s.m.sandboxesCreated.WithLabelValues(name, source).Inc()
	s.m.createSeconds.WithLabelValues(source).Observe(time.Since(start).Seconds())

	if err := s.audit.RecordSync(r.Context(), audit.Event{
		Type: audit.TypeSandboxCreate, Sandbox: sb.ID, Token: tid,
		Attrs: map[string]any{
			"template": name, "source": source, "networked": req.Networked,
			"ttl_s": int64(ttl / time.Second), "guest_ip": sb.GuestIP,
		},
	}); err != nil {
		// The audit record is a hard requirement for a boundary event.
		// Roll the sandbox back when the write fails.
		s.reg.remove(sb.ID)
		s.reg.release(tid)
		s.m.sandboxesActive.WithLabelValues(name).Dec()
		// counter cannot be decremented, a failed create is rare
		s.asyncDestroy(sb)
		writeErr(w, http.StatusServiceUnavailable, "audit write failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, view(sb))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	tid := tokenID(r)
	var out []sandboxView
	for _, sb := range s.reg.list() {
		if sb.TokenID == tid {
			out = append(out, view(sb))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sb := s.mustOwn(w, r)
	if sb == nil {
		return
	}
	writeJSON(w, http.StatusOK, view(sb))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sb := s.mustOwn(w, r)
	if sb == nil {
		return
	}
	s.reg.remove(sb.ID)
	s.reg.release(sb.TokenID)
	s.m.sandboxesActive.WithLabelValues(sb.Template).Dec()
	// Drain the async writer first so exec and file events sort before destroy.
	s.audit.Flush()
	s.recordDestroy(r.Context(), sb, "deleted")
	s.asyncDestroy(sb)
	w.WriteHeader(http.StatusNoContent)
}

type execReq struct {
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env"`
	Dir          string            `json:"dir"`
	Stdin        string            `json:"stdin"` // base64
	TTY          bool              `json:"tty"`
	TimeoutMS    int               `json:"timeout_ms"`
	AllowSecrets bool              `json:"allow_secrets"` // pass secret-flagged env/stdin
}

// ingressDecision is the secret firewall's verdict on one blob.
type ingressDecision struct {
	action   string // clean, allow, redact, block
	content  []byte // the bytes to use (redacted when action == "redact")
	findings []secretscan.Finding
	// recErr holds the RecordSync error for an allow or a redact decision.
	// A non-nil value means flagged content has no durable audit record.
	// The caller must fail the request closed.
	recErr error
}

// screen runs the secret firewall over data. name is the target (a guest path,
// or "env:KEY", or "stdin"), used for logging and path allowlists. allowReq is
// the request's allow_secrets flag.
func (s *Server) screen(tid, name string, data []byte, allowReq bool) ingressDecision {
	if s.scanner == nil {
		return ingressDecision{action: "clean", content: data}
	}
	fs := s.scanner.Scan(name, data)
	if len(fs) == 0 {
		return ingressDecision{action: "clean", content: data}
	}
	action, content := "block", data
	switch {
	case s.cfg.SecretRedact:
		action, content = "redact", s.scanner.Redact(data, fs)
	case allowReq && s.cfg.Policies[tid].AllowSecretOverride:
		action = "allow"
	}
	for _, f := range fs {
		s.m.secretFindings.WithLabelValues(f.Rule, action).Inc()
	}
	rules := map[string]bool{}
	for _, f := range fs {
		rules[f.Rule] = true
	}
	list := make([]string, 0, len(rules))
	for r := range rules {
		list = append(list, r)
	}
	s.log.WithFields(logrus.Fields{
		"token": tid, "target": name, "findings": len(fs),
		"rules": strings.Join(list, ","), "action": action,
	}).Warn("secret_detected")

	auditType := audit.TypeSecretBlock
	switch action {
	case "allow":
		auditType = audit.TypeSecretAllow
	case "redact":
		auditType = audit.TypeSecretRedact
	}
	recErr := s.audit.RecordSync(context.Background(), audit.Event{
		Type: auditType, Token: tid,
		Attrs: map[string]any{"target": name, "rules": list, "findings": len(fs)},
	})
	return ingressDecision{action: action, content: content, findings: fs, recErr: recErr}
}

// auditUnrecorded reports whether an allow or a redact decision passed content
// with no durable audit record. The caller must refuse the request.
func (d ingressDecision) auditUnrecorded() bool {
	return (d.action == "allow" || d.action == "redact") && d.recErr != nil
}

// writeSecretBlock writes the 409 for blocked ingress content.
func writeSecretBlock(w http.ResponseWriter, where string, fs []secretscan.Finding) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":    "secret detected in " + where + " — not sent",
		"hint":     "remove the credential, or resend with allow_secrets if your token permits it",
		"findings": fs,
	})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	sb := s.mustOwn(w, r)
	if sb == nil {
		return
	}
	start := time.Now()
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return
	}
	if len(req.Argv) == 0 {
		writeErr(w, http.StatusBadRequest, "argv is required")
		return
	}
	stdin, err := base64.StdEncoding.DecodeString(req.Stdin)
	if req.Stdin != "" && err != nil {
		writeErr(w, http.StatusBadRequest, "stdin must be base64")
		return
	}

	// Secret firewall: screen every env value and stdin before the command
	// runs. This is before the egress-proxy env below, so injected values are
	// not scanned.
	if s.scanner != nil {
		tid := tokenID(r)
		for k, v := range req.Env {
			// Scan the assignment form so NAME=value rules can fire.
			d := s.screen(tid, "env:"+k, []byte(k+"="+v), req.AllowSecrets)
			if d.action == "block" {
				writeSecretBlock(w, "env var "+k, d.findings)
				return
			}
			if d.auditUnrecorded() {
				writeErr(w, http.StatusServiceUnavailable, "audit write failed — request refused")
				return
			}
			if d.action == "redact" {
				req.Env[k] = strings.TrimPrefix(string(d.content), k+"=")
			}
		}
		if len(stdin) > 0 {
			d := s.screen(tid, "stdin", stdin, req.AllowSecrets)
			if d.action == "block" {
				writeSecretBlock(w, "stdin", d.findings)
				return
			}
			if d.auditUnrecorded() {
				writeErr(w, http.StatusServiceUnavailable, "audit write failed — request refused")
				return
			}
			if d.action == "redact" {
				stdin = d.content
			}
		}
	}

	// Networked sandboxes must send outbound HTTP through the egress proxy so
	// the domain policy applies. The server sets this; a caller cannot override.
	if sb.net != nil && s.cfg.EgressPort > 0 {
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		proxy := fmt.Sprintf("http://%s:%d", sb.net.VethHostIP, s.cfg.EgressPort)
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			req.Env[k] = proxy
		}
		req.Env["NO_PROXY"] = "localhost,127.0.0.1"
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	send := func(v any) { _ = enc.Encode(v); flush() }

	res, err := sb.client().Exec(r.Context(), agentclient.ExecOptions{
		Argv:    req.Argv,
		Env:     req.Env,
		Dir:     req.Dir,
		Stdin:   stdin,
		TTY:     req.TTY,
		Timeout: time.Duration(req.TimeoutMS) * time.Millisecond,
	}, streamWriter{send, "stdout"}, streamWriter{send, "stderr"})
	if err != nil {
		s.m.execTotal.WithLabelValues("error").Inc()
	} else {
		outcome := "ok"
		if res.Code != 0 {
			outcome = "nonzero"
		}
		s.m.execTotal.WithLabelValues(outcome).Inc()
	}

	argv := clampArgv(req.Argv, 4096)
	envKeys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if k == "HTTP_PROXY" || k == "HTTPS_PROXY" || k == "http_proxy" || k == "https_proxy" || k == "NO_PROXY" {
			continue // the server injects these proxy variables, they are not caller data
		}
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	execAttrs := map[string]any{
		"argv": argv, "env_keys": envKeys, "dir": req.Dir, "tty": req.TTY,
		"dur_ms": time.Since(start).Milliseconds(),
	}
	if err != nil {
		execAttrs["error"] = err.Error()
	} else {
		execAttrs["exit"] = res.Code
		execAttrs["signal"] = res.Signal
		execAttrs["timed_out"] = res.TimedOut
	}
	s.audit.Record(audit.Event{Type: audit.TypeExec, Sandbox: sb.ID, Token: tokenID(r), Attrs: execAttrs})

	if err != nil {
		send(map[string]any{"stream": "error", "message": err.Error()})
		return
	}
	send(map[string]any{"stream": "exit", "code": res.Code, "signal": res.Signal, "timed_out": res.TimedOut})
}

// clampArgv returns argv with its total byte size limited to max. It appends a
// marker when it removes the extra arguments.
func clampArgv(argv []string, max int) []string {
	total := 0
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if total+len(a) > max {
			out = append(out, "…")
			break
		}
		total += len(a)
		out = append(out, a)
	}
	return out
}

type streamWriter struct {
	send   func(any)
	stream string
}

func (sw streamWriter) Write(b []byte) (int, error) {
	sw.send(map[string]any{"stream": sw.stream, "data": base64.StdEncoding.EncodeToString(b)})
	return len(b), nil
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	sb := s.mustOwn(w, r)
	if sb == nil {
		return
	}
	path := "/" + strings.TrimPrefix(r.PathValue("path"), "/")

	scanAction := "clean"
	var body io.Reader = r.Body
	if s.scanner != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, s.maxUp+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		if int64(len(raw)) > s.maxUp {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file larger than the %d-byte scan limit", s.maxUp))
			return
		}
		allow := r.URL.Query().Get("allow_secrets") == "true"
		d := s.screen(tokenID(r), path, raw, allow)
		scanAction = d.action
		if d.action == "block" {
			// Record that the write did not happen. The event holds no file
			// content.
			s.audit.Record(audit.Event{Type: audit.TypeFilePut, Sandbox: sb.ID, Token: tokenID(r),
				Attrs: map[string]any{"path": path, "bytes": 0, "scan": "block"}})
			writeSecretBlock(w, path, d.findings)
			return
		}
		if d.auditUnrecorded() {
			writeErr(w, http.StatusServiceUnavailable, "audit write failed — request refused")
			return
		}
		body = bytes.NewReader(d.content)
	}

	res, err := sb.client().WriteFile(r.Context(), path, 0o644, body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "write file: "+err.Error())
		return
	}
	s.m.fileBytes.WithLabelValues("in").Add(float64(res.BytesWritten))
	s.audit.Record(audit.Event{Type: audit.TypeFilePut, Sandbox: sb.ID, Token: tokenID(r),
		Attrs: map[string]any{"path": path, "bytes": res.BytesWritten, "sha256": res.SHA256, "scan": scanAction}})
	writeJSON(w, http.StatusOK, map[string]any{"bytes_written": res.BytesWritten, "sha256": res.SHA256})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	sb := s.mustOwn(w, r)
	if sb == nil {
		return
	}
	path := "/" + strings.TrimPrefix(r.PathValue("path"), "/")
	w.Header().Set("Content-Type", "application/octet-stream")
	counter := &countingWriter{w: w}
	res, err := sb.client().ReadFile(r.Context(), path, counter)
	if err != nil {
		if counter.n == 0 {
			writeErr(w, http.StatusNotFound, "read file: "+err.Error())
		}
		return
	}
	s.m.fileBytes.WithLabelValues("out").Add(float64(res.Size))
	s.audit.Record(audit.Event{Type: audit.TypeFileGet, Sandbox: sb.ID, Token: tokenID(r),
		Attrs: map[string]any{"path": path, "bytes": res.Size, "sha256": res.SHA256}})
	w.Header().Set("X-Sha256", res.SHA256)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.audit.Enabled() {
		writeErr(w, http.StatusNotFound, "audit log is disabled on this server")
		return
	}
	tid := tokenID(r)
	all := s.cfg.Policies[tid].auditAll()

	q := r.URL.Query()
	f := audit.Filter{
		Sandbox: q.Get("sandbox"),
		Types:   q["type"],
		Limit:   atoiOr(q.Get("limit"), 1000),
		Desc:    q.Get("order") == "desc",
	}
	if all {
		f.Token = q.Get("token")
	}
	if since := q.Get("since"); since != "" {
		ts, ok := parseWhen(since)
		if !ok {
			writeErr(w, http.StatusBadRequest, "bad since: use RFC3339 or a negative Go duration like -15m")
			return
		}
		f.Since = ts
	}
	if until := q.Get("until"); until != "" {
		ts, ok := parseWhen(until)
		if !ok {
			writeErr(w, http.StatusBadRequest, "bad until: use RFC3339 or a negative Go duration like -15m")
			return
		}
		f.Until = ts
	}

	evs, err := s.audit.LiveQuery(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read audit log: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, e := range evs {
		if !all && !s.auditVisible(tid, e) {
			continue
		}
		_ = enc.Encode(auditWire(e))
	}
}

// handleReport returns the rollup for one sandbox. The owner or an "all"
// token may read it. The report is built from the audit files, so it works
// after the sandbox stops.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if !s.audit.Enabled() {
		writeErr(w, http.StatusNotFound, "audit log is disabled on this server")
		return
	}
	id := r.PathValue("id")
	tid := tokenID(r)
	if !s.cfg.Policies[tid].auditAll() {
		owner, ok := s.audit.SandboxToken(id)
		if !ok || owner != tid {
			writeErr(w, http.StatusNotFound, "no such sandbox")
			return
		}
	}
	rep, err := s.audit.LiveReport(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "build report: "+err.Error())
		return
	}
	if rep.Sandbox == "" {
		writeErr(w, http.StatusNotFound, "no such sandbox")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// auditVisible reports whether tid may see event e without the "all" scope.
func (s *Server) auditVisible(tid string, e audit.Event) bool {
	if e.Token == tid {
		return true
	}
	if e.Sandbox != "" {
		if owner, ok := s.audit.SandboxToken(e.Sandbox); ok && owner == tid {
			return true
		}
	}
	return false
}

// auditWire is the JSON shape returned to API clients.
func auditWire(e audit.Event) map[string]any {
	return map[string]any{
		"seq": e.Seq, "ts": e.TS.UTC().Format(time.RFC3339Nano), "type": e.Type,
		"sandbox": e.Sandbox, "token": e.Token, "attrs": e.Attrs,
		"prev": e.Prev, "hash": e.Hash,
	}
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// parseWhen accepts RFC3339 or a Go duration with a leading "-" (e.g. -15m),
// which it subtracts from now. The CLI offline path shares audit.ParseWhen.
func parseWhen(s string) (time.Time, bool) { return audit.ParseWhen(s) }

// mustOwn resolves the {id} path value and checks the caller owns it.
func (s *Server) mustOwn(w http.ResponseWriter, r *http.Request) *Sandbox {
	sb, ok := s.reg.get(r.PathValue("id"))
	if !ok || sb.TokenID != tokenID(r) {
		writeErr(w, http.StatusNotFound, "no such sandbox")
		return nil
	}
	return sb
}

// janitor destroys sandboxes past their TTL.
func (s *Server) janitor() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.janitorStop:
			return
		case now := <-t.C:
			for _, sb := range s.reg.list() {
				if now.After(sb.ExpiresAt) {
					s.log.WithField("id", sb.ID).Info("sandbox TTL expired")
					s.reg.remove(sb.ID)
					s.reg.release(sb.TokenID)
					s.m.sandboxesActive.WithLabelValues(sb.Template).Dec()
					// Drain the async writer first so earlier events sort before destroy.
					s.audit.Flush()
					s.recordDestroy(context.Background(), sb, "ttl")
					s.asyncDestroy(sb)
				}
			}
		}
	}
}

// --- helpers ---

// templateNames returns the name of each template, for the daemon.start event.
func templateNames(ts []Template) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// recordDestroy emits the sandbox.destroy audit event. reason is one of
// deleted, ttl, idle, shutdown, orphan.
func (s *Server) recordDestroy(ctx context.Context, sb *Sandbox, reason string) {
	life := int64(time.Since(sb.CreatedAt) / time.Second)
	_ = s.audit.RecordSync(ctx, audit.Event{
		Type: audit.TypeSandboxDestroy, Sandbox: sb.ID, Token: sb.TokenID,
		Attrs: map[string]any{"reason": reason, "lifetime_s": life},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func statusClass(code int) string { return fmt.Sprintf("%dxx", code/100) }

func routeLabel(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/healthz", p == "/metrics", p == "/v1/sandboxes", p == "/v1/audit":
		return r.Method + " " + p
	case strings.HasPrefix(p, "/v1/sandboxes/"):
		rest := strings.TrimPrefix(p, "/v1/sandboxes/")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return r.Method + " /v1/sandboxes/{id}/" + strings.SplitN(rest[i+1:], "/", 2)[0]
		}
		return r.Method + " /v1/sandboxes/{id}"
	default:
		return r.Method + " other"
	}
}
