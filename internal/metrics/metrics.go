// Package metrics is the one place a Prometheus collector is declared. Nothing
// else in the tree imports the client library, so cardinality stays reviewable
// in one file.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector the service exports. It is passed explicitly
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
	ProviderCooldown *prometheus.GaugeVec

	PushDelivery *prometheus.CounterVec

	CacheEvents *prometheus.CounterVec

	TempDiskBytes prometheus.Gauge

	SearchDuration *prometheus.HistogramVec
	SearchResults  *prometheus.CounterVec

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
			Help: "HTTP requests by route pattern, method and status class.",
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
			Help: "Messages sent to a retry queue, by queue.",
		}, []string{"queue"}),

		DeadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_queue_dead_lettered_total",
			Help: "Messages sent to a dead-letter queue, by queue.",
		}, []string{"queue"}),

		OutboxAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_outbox_oldest_pending_seconds",
			Help: "Age of the oldest unpublished outbox event. The publisher falling behind shows up here first.",
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
			Help: "Provider call failures by provider and failure class.",
		}, []string{"provider", "class"}),

		ProviderCooldown: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reelpin_provider_cooldown_active",
			Help: "1 while a provider is in cooldown and its calls are being skipped.",
		}, []string{"provider"}),

		PushDelivery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_push_delivery_total",
			Help: "Push notification delivery attempts by outcome.",
		}, []string{"outcome"}),

		CacheEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_cache_events_total",
			Help: "Read-through cache hits, misses and errors by kind.",
		}, []string{"kind", "event"}),

		TempDiskBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_worker_temp_bytes",
			Help: "Bytes the worker is holding in its temp directory.",
		}),

		SearchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "reelpin_search_duration_seconds",
			Help:    "Search latency by the arms that ran.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"mode"}),

		SearchResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reelpin_search_results_total",
			Help: "Searches by whether they returned anything. A rising empty rate is a relevance regression.",
		}, []string{"outcome"}),

		LiveWorkers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reelpin_live_workers",
			Help: "Workers whose heartbeat has not expired.",
		}),
	}

	registry.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.DatabasePool,
		m.QueuePublished, m.QueueConsumed, m.QueueRetried, m.DeadLettered,
		m.OutboxAgeSeconds, m.OldestQueuedJobAge,
		m.StageDuration, m.StageResults,
		m.ProviderFailures, m.ProviderCooldown,
		m.PushDelivery, m.CacheEvents, m.TempDiskBytes,
		m.SearchDuration, m.SearchResults, m.LiveWorkers,
	)
	return m
}

// Handler serves the exposition format. It is mounted outside the JSON API,
// because Prometheus wants text and every API middleware wants JSON.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
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

// ObserveSearch records one search and whether it found anything.
func (m *Metrics) ObserveSearch(mode string, results int, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.SearchDuration.WithLabelValues(mode).Observe(elapsed.Seconds())
	outcome := "results"
	if results == 0 {
		outcome = "empty"
	}
	m.SearchResults.WithLabelValues(outcome).Inc()
}

// ObserveStage records one pipeline stage attempt. Outcome is "ok" or the
// failure class, so a rising internal rate is visible next to a rising
// transient one.
func (m *Metrics) ObserveStage(stage, platform, outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.StageDuration.WithLabelValues(stage, platform).Observe(elapsed.Seconds())
	m.StageResults.WithLabelValues(stage, outcome).Inc()
}
