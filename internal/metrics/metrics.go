// Package metrics is the one place a Prometheus collector is declared. Nothing
// else in the tree imports the client library, so cardinality stays reviewable
// in one file.
package metrics

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector a process exports. It is passed explicitly
// rather than kept in a package global, so a test can build its own.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec

	DatabasePool *prometheus.GaugeVec

	QueuePublished *prometheus.CounterVec
	QueueConsumed  *prometheus.CounterVec
	QueueRetried   *prometheus.CounterVec
	DeadLettered   *prometheus.CounterVec

	OutboxAgeSeconds   prometheus.Gauge
	OldestQueuedJobAge prometheus.Gauge

	StageDuration *prometheus.HistogramVec
	StageResults  *prometheus.CounterVec

	ProviderFailures *prometheus.CounterVec
	SearchDuration   *prometheus.HistogramVec
	SearchResults    *prometheus.CounterVec

	PushDelivery *prometheus.CounterVec

	TempDiskBytes prometheus.Gauge

	LiveWorkers prometheus.Gauge
}

// New builds the registry and every collector on it. It also registers the Go
// runtime and process collectors, which is where memory, goroutines and file
// descriptors come from.
func New() *Metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: registry,

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_http_requests_total",
			Help: "HTTP requests by route pattern, method and status.",
		}, []string{"route", "method", "status"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "reelpin_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern and method.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),

		DatabasePool: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reelpin_db_pool_connections",
			Help: "pgx pool connections by state.",
		}, []string{"state"}),

		QueuePublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_queue_published_total",
			Help: "Messages published, by routing key and whether the broker confirmed.",
		}, []string{"routing_key", "outcome"}),

		QueueConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_queue_consumed_total",
			Help: "Messages consumed, by queue and outcome.",
		}, []string{"queue", "outcome"}),

		QueueRetried: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_queue_retried_total",
			Help: "Messages parked in a backoff queue, by queue.",
		}, []string{"queue"}),

		DeadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_queue_dead_lettered_total",
			Help: "Messages rejected without requeue, by queue.",
		}, []string{"queue"}),

		OutboxAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_outbox_oldest_pending_seconds",
			Help: "Age of the oldest unpublished outbox event. The dispatcher falling behind shows up here first.",
		}),

		OldestQueuedJobAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_jobs_oldest_queued_seconds",
			Help: "Age of the oldest processing job still waiting for a worker.",
		}),

		StageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "reelpin_pipeline_stage_duration_seconds",
			Help:    "Pipeline stage latency by stage and platform.",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"stage", "platform"}),

		StageResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_pipeline_stage_results_total",
			Help: "Pipeline stage outcomes by stage and failure class.",
		}, []string{"stage", "outcome"}),

		ProviderFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_provider_failures_total",
			Help: "Stage failures blamed on a provider, by platform and failure class.",
		}, []string{"platform", "class"}),

		SearchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "reelpin_search_duration_seconds",
			Help:    "Search latency by the arms that answered.",
			Buckets: prometheus.DefBuckets,
		}, []string{"mode"}),

		SearchResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_search_results_total",
			Help: "Searches by whether they returned anything.",
		}, []string{"mode", "outcome"}),

		PushDelivery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_push_delivery_total",
			Help: "Push notification delivery attempts by outcome.",
		}, []string{"outcome"}),

		TempDiskBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_worker_temp_bytes",
			Help: "Bytes the worker is holding in its temp directory.",
		}),

		LiveWorkers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_live_workers",
			Help: "Workers whose heartbeat has not expired.",
		}),
	}

	registry.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.DatabasePool,
		m.QueuePublished, m.QueueConsumed, m.QueueRetried, m.DeadLettered,
		m.OutboxAgeSeconds, m.OldestQueuedJobAge,
		m.StageDuration, m.StageResults, m.ProviderFailures,
		m.SearchDuration, m.SearchResults,
		m.PushDelivery, m.TempDiskBytes, m.LiveWorkers,
	)
	return m
}

// GuardedHandler serves the exposition format to callers that present the
// admin key. Queue depths and failure rates are operational detail, so the
// endpoint is not public; it answers plain text, because it is deliberately
// outside the JSON contract.
func (m *Metrics) GuardedHandler(adminKey string) http.Handler {
	exposition := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := []byte(r.Header.Get("X-Admin-Key"))
		if subtle.ConstantTimeCompare(presented, []byte(adminKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		exposition.ServeHTTP(w, r)
	})
}

// ObserveRequest records one finished request. The route is the mux pattern,
// never the raw path: a path carries reel ids and would grow a new time series
// per reel.
func (m *Metrics) ObserveRequest(route, method string, status int, elapsed time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	m.RequestsTotal.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.RequestDuration.WithLabelValues(route, method).Observe(elapsed.Seconds())
}

// ObserveSearch records one finished search. The outcome is "empty" or "hit"
// rather than the count, because the question the alert asks is how often the
// library answers nothing, and a count would be a label per result.
func (m *Metrics) ObserveSearch(mode string, total int, elapsed time.Duration) {
	if m == nil {
		return
	}
	outcome := "hit"
	if total == 0 {
		outcome = "empty"
	}
	m.SearchDuration.WithLabelValues(mode).Observe(elapsed.Seconds())
	m.SearchResults.WithLabelValues(mode, outcome).Inc()
}

// ObserveStage records one pipeline stage attempt. Outcome is "ok" or the
// failure class, so a rising internal rate is visible next to a rising
// transient one without any error text reaching a label.
func (m *Metrics) ObserveStage(stage, platform, outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.StageDuration.WithLabelValues(stage, platform).Observe(elapsed.Seconds())
	m.StageResults.WithLabelValues(stage, outcome).Inc()
}
