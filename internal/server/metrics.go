package server

import "github.com/prometheus/client_golang/prometheus"

// metrics holds every collector the daemon exports at /metrics.
type metrics struct {
	sandboxesActive  *prometheus.GaugeVec
	sandboxesCreated *prometheus.CounterVec
	createSeconds    *prometheus.HistogramVec
	execTotal        *prometheus.CounterVec
	fileBytes        *prometheus.CounterVec
	secretFindings   *prometheus.CounterVec
	quotaRejects     prometheus.Counter
	httpRequests     *prometheus.CounterVec
	httpSeconds      *prometheus.HistogramVec
	poolReady        prometheus.GaugeFunc
	auditEvents      *prometheus.CounterVec
	auditDropped     prometheus.Counter
	auditWrite       *prometheus.HistogramVec
	auditQueue       prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer, poolReady func() float64) *metrics {
	m := &metrics{
		sandboxesActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "moat_sandboxes_active",
			Help: "Sandboxes that exist right now, by template.",
		}, []string{"template"}),
		sandboxesCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_sandboxes_created_total",
			Help: "Sandboxes created, by template and source (pool or cold).",
		}, []string{"template", "source"}),
		createSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "moat_sandbox_create_seconds",
			Help:    "Time from create request to a ready sandbox.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 5},
		}, []string{"source"}),
		execTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_exec_total",
			Help: "Exec calls, by outcome (ok, nonzero, error).",
		}, []string{"outcome"}),
		fileBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_file_bytes_total",
			Help: "Bytes transferred to and from sandboxes, by direction.",
		}, []string{"direction"}),
		secretFindings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_secret_findings_total",
			Help: "Secrets found in ingress content, by rule and action (block, redact, allow).",
		}, []string{"rule", "action"}),
		quotaRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "moat_quota_rejects_total",
			Help: "Create requests refused because a quota was full.",
		}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_http_requests_total",
			Help: "HTTP requests, by route and status class.",
		}, []string{"route", "status"}),
		httpSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "moat_http_request_seconds",
			Help:    "HTTP request duration, by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		auditEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moat_audit_events_total",
			Help: "Audit events recorded, by type and result (ok, error).",
		}, []string{"type", "result"}),
		auditDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "moat_audit_dropped_total",
			Help: "Async audit events dropped because the buffer was full or a write failed.",
		}),
		auditWrite: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "moat_audit_write_seconds",
			Help:    "Audit append and fsync latency, by path (sync, async).",
			Buckets: []float64{.0001, .0005, .001, .005, .01, .05, .1, .5},
		}, []string{"path"}),
		auditQueue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "moat_audit_queue_depth",
			Help: "Audit async buffer occupancy.",
		}),
	}
	m.poolReady = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "moat_pool_ready",
		Help: "Warm pool members ready to hand out.",
	}, poolReady)

	reg.MustRegister(
		m.sandboxesActive, m.sandboxesCreated, m.createSeconds, m.execTotal,
		m.fileBytes, m.secretFindings, m.quotaRejects, m.httpRequests, m.httpSeconds, m.poolReady,
		m.auditEvents, m.auditDropped, m.auditWrite, m.auditQueue,
	)
	return m
}

// promAuditMetrics adapts the server metrics to the audit.Metrics interface.
type promAuditMetrics struct{ m *metrics }

func (a promAuditMetrics) Recorded(typ, result string) {
	a.m.auditEvents.WithLabelValues(typ, result).Inc()
}
func (a promAuditMetrics) Dropped() { a.m.auditDropped.Inc() }
func (a promAuditMetrics) WriteObserved(p string, s float64) {
	a.m.auditWrite.WithLabelValues(p).Observe(s)
}
func (a promAuditMetrics) QueueDepth(n int) { a.m.auditQueue.Set(float64(n)) }
