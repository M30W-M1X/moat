// Package egress is a forward HTTP proxy that enforces a per-deployment domain
// policy on sandbox outbound traffic. It handles CONNECT (HTTPS tunnels, filtered
// by the requested host name, no TLS interception) and absolute-URI HTTP
// requests. Networked sandboxes get HTTP_PROXY / HTTPS_PROXY pointed here.
package egress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/M30W-M1X/moat/internal/audit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// Policy decides which destination host names a sandbox may reach.
//
// A host is allowed when it does not match Deny and (Allow is empty or it
// matches Allow). Matching is case-insensitive and works on the exact name or
// any parent domain: "example.com" matches "example.com" and "api.example.com".
type Policy struct {
	Allow []string
	Deny  []string
}

// Match reports whether host (no port) is reachable under the policy and names
// the rule that decided it. The rule is a Deny or Allow pattern, or
// "default-allow" when Allow is empty, or "default-deny" when Allow is set and
// nothing matched.
func (p Policy) Match(host string) (rule string, allowed bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range p.Deny {
		if hostMatches(host, d) {
			return d, false
		}
	}
	if len(p.Allow) == 0 {
		return "default-allow", true
	}
	for _, a := range p.Allow {
		if hostMatches(host, a) {
			return a, true
		}
	}
	return "default-deny", false
}

// Permits reports whether host (no port) is reachable under the policy.
func (p Policy) Permits(host string) bool {
	_, ok := p.Match(host)
	return ok
}

func hostMatches(host, pattern string) bool {
	pattern = strings.ToLower(strings.TrimPrefix(pattern, "*."))
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

// auditHook holds the audit sink and the client-IP resolver together, so
// SetAudit swaps both with one atomic store.
type auditHook struct {
	rec      *audit.Recorder
	identify func(netip.Addr) (string, string, bool)
}

// policyHook holds a per-token policy resolver. SetPolicyResolver swaps it
// atomically so concurrent requests never observe a torn function value.
type policyHook struct {
	resolve func(tokenID string) (Policy, bool)
}

// Proxy is the forward proxy handler.
type Proxy struct {
	pol       Policy
	log       *logrus.Logger
	dial      *net.Dialer
	rt        http.RoundTripper
	decision  *prometheus.CounterVec
	audit     atomic.Pointer[auditHook]
	policyRes atomic.Pointer[policyHook]
}

// SetAudit attaches an audit recorder and a client-IP resolver. It is safe to
// call while the proxy serves traffic. The resolver maps a proxy client IP to
// its sandbox id and token id.
func (p *Proxy) SetAudit(rec *audit.Recorder, identify func(netip.Addr) (string, string, bool)) {
	p.audit.Store(&auditHook{rec: rec, identify: identify})
}

// SetPolicyResolver attaches a per-token egress policy lookup. When a request
// resolves to a token id with a policy, that policy applies alongside the
// global Deny list. It is safe to call while the proxy serves traffic.
func (p *Proxy) SetPolicyResolver(fn func(tokenID string) (Policy, bool)) {
	p.policyRes.Store(&policyHook{resolve: fn})
}

// New builds a Proxy. reg may be nil.
func New(pol Policy, log *logrus.Logger, reg prometheus.Registerer) *Proxy {
	d := &net.Dialer{Timeout: 10 * time.Second}
	p := &Proxy{
		pol:  pol,
		log:  log,
		dial: d,
		rt:   &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: false},
		decision: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_egress_requests_total",
			Help: "Egress proxy requests by decision (allow, deny).",
		}, []string{"decision"}),
	}
	if reg != nil {
		reg.MustRegister(p.decision)
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}
	if !p.decide(r, host, "proxy") {
		http.Error(w, "egress to "+host+" is not allowed", http.StatusForbidden)
		return
	}
	upstream, err := p.dial.DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "dial "+host+": "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	pipe(client, upstream)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "this is a forward proxy; use an absolute URL", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	if !p.decide(r, host, "proxy") {
		http.Error(w, "egress to "+host+" is not allowed", http.StatusForbidden)
		return
	}
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	resp, err := p.rt.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) decide(r *http.Request, host string, via string) bool {
	rule, ok, sandbox, token, clientIP := p.evalPolicy(r.RemoteAddr, host)
	d := "allow"
	if !ok {
		d = "deny"
	}
	p.decision.WithLabelValues(d).Inc()
	p.log.WithFields(logrus.Fields{"host": host, "method": r.Method, "decision": d}).Info("egress")
	p.recordEgressResolved(r, host, ok, via, rule, sandbox, token, clientIP)
	return ok
}

// evalPolicy resolves the client sandbox and token, then checks the global
// Deny list, the per-token policy when one exists, and the global Allow list.
// A per-token policy can never weaken the global Deny list.
func (p *Proxy) evalPolicy(remoteAddr, host string) (rule string, allowed bool, sandbox, token, clientIP string) {
	if h := p.audit.Load(); h != nil && h.identify != nil {
		if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
			if sid, tid, ok := h.identify(ap.Addr()); ok {
				sandbox, token = sid, tid
			} else {
				clientIP = ap.Addr().String()
			}
		}
	}

	norm := strings.ToLower(strings.TrimSuffix(host, "."))
	// Global deny list always wins.
	for _, d := range p.pol.Deny {
		if hostMatches(norm, d) {
			return d, false, sandbox, token, clientIP
		}
	}

	if ph := p.policyRes.Load(); ph != nil && ph.resolve != nil && token != "" {
		if tokPol, ok := ph.resolve(token); ok {
			for _, d := range tokPol.Deny {
				if hostMatches(norm, d) {
					return d, false, sandbox, token, clientIP
				}
			}
			if len(tokPol.Allow) > 0 {
				for _, a := range tokPol.Allow {
					if hostMatches(norm, a) {
						return a, true, sandbox, token, clientIP
					}
				}
				return "default-deny", false, sandbox, token, clientIP
			}
		}
	}

	rule, allowed = p.pol.Match(norm)
	return rule, allowed, sandbox, token, clientIP
}

// recordEgressResolved writes one egress audit event with pre-resolved client
// identity. Both allow and deny events go on the async path so a guest cannot
// flood the synchronous writer. The event holds metadata only.
func (p *Proxy) recordEgressResolved(r *http.Request, host string, allowed bool, via, rule, sandbox, token, clientIP string) {
	h := p.audit.Load()
	if h == nil || h.rec == nil || !h.rec.Enabled() {
		return
	}
	port := 80
	if r.Method == http.MethodConnect {
		port = 443
	}
	if _, ps, err := net.SplitHostPort(r.Host); err == nil {
		if n, err := strconv.Atoi(ps); err == nil {
			port = n
		}
	}
	method := r.Method
	if method == http.MethodConnect {
		method = "CONNECT"
	}
	attrs := map[string]any{
		"host":         host,
		"port":         port,
		"method":       method,
		"matched_rule": rule,
		"via":          via,
	}
	if sandbox == "" && clientIP != "" {
		attrs["client_ip"] = clientIP
	}
	ev := audit.Event{Sandbox: sandbox, Token: token, Attrs: attrs}
	if allowed {
		ev.Type = audit.TypeEgressAllow
	} else {
		ev.Type = audit.TypeEgressDeny
	}
	h.rec.Record(ev)
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// Serve runs the proxy until ctx is done and returns the address it listened on.
func Serve(ctx context.Context, addr string, p *Proxy) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.WithError(err).Warn("egress proxy stopped")
		}
	}()
	return ln.Addr().String(), nil
}
