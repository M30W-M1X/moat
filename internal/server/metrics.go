package server

import "github.com/prometheus/client_golang/prometheus"

// metrics holds every collector the daemon exports at /metrics.
type metrics struct {
	sandboxesActive  *prometheus.GaugeVec
	sandboxesCreated *prometheus.CounterVec
	createSeconds    *prometheus.HistogramVec
	execTotal        *prometheus.CounterVec
	fileBytes        *prometheus.CounterVec
	quotaRejects     prometheus.Counter
	httpRequests     *prometheus.CounterVec
	httpSeconds      *prometheus.HistogramVec
	poolReady        prometheus.GaugeFunc
}

func newMetrics(reg prometheus.Registerer, poolReady func() float64) *metrics {
	m := &metrics{
		sandboxesActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "microvmd_sandboxes_active",
			Help: "Sandboxes that exist right now, by template.",
		}, []string{"template"}),
		sandboxesCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "microvmd_sandboxes_created_total",
			Help: "Sandboxes created, by template and source (pool or cold).",
		}, []string{"template", "source"}),
		createSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "microvmd_sandbox_create_seconds",
			Help:    "Time from create request to a ready sandbox.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 5},
		}, []string{"source"}),
		execTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "microvmd_exec_total",
			Help: "Exec calls, by outcome (ok, nonzero, error).",
		}, []string{"outcome"}),
		fileBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "microvmd_file_bytes_total",
			Help: "Bytes transferred to and from sandboxes, by direction.",
		}, []string{"direction"}),
		quotaRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "microvmd_quota_rejects_total",
			Help: "Create requests refused because a quota was full.",
		}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "microvmd_http_requests_total",
			Help: "HTTP requests, by route and status class.",
		}, []string{"route", "status"}),
		httpSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "microvmd_http_request_seconds",
			Help:    "HTTP request duration, by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
	}
	m.poolReady = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "microvmd_pool_ready",
		Help: "Warm pool members ready to hand out.",
	}, poolReady)

	reg.MustRegister(
		m.sandboxesActive, m.sandboxesCreated, m.createSeconds, m.execTotal,
		m.fileBytes, m.quotaRejects, m.httpRequests, m.httpSeconds, m.poolReady,
	)
	return m
}
