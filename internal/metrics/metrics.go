// Package metrics provides Prometheus metrics exposition for monitoring and
// observability. All metrics are automatically registered with the default
// Prometheus registry and exposed via the standard /metrics HTTP endpoint.
//
// Metric Categories:
//   - Queue metrics: Message counts by status, queue depth
//   - Delivery metrics: Attempt counts, success/failure rates, latency histograms
//   - Callback metrics: Webhook delivery attempts, durations by outcome
//   - HTTP metrics: Request counts, status codes, latency by endpoint
//   - Circuit breaker metrics: State tracking, transition counts
//   - IP reputation metrics: Degraded IP tracking, reputation events
//   - Database metrics: File sizes, connection pool stats, query durations
//
// Example Usage:
//
//	m := metrics.NewMetrics()
//
//	// Record delivery attempt
//	start := time.Now()
//	m.RecordDeliveryAttempt("success", time.Since(start).Seconds())
//
//	// Update queue depth
//	m.RecordQueueDepth("queued", 42)
//
//	// Track circuit breaker state
//	m.SetCircuitBreakerState(2) // 2 = open
//
// Prometheus Exposition:
//
//	import "github.com/prometheus/client_golang/prometheus/promhttp"
//
//	http.Handle("/metrics", promhttp.Handler())
//	http.ListenAndServe(":9090", nil)
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the application. Each metric is
// pre-registered with the default Prometheus registry using promauto.
type Metrics struct {
	// Queue metrics
	QueueDepth *prometheus.GaugeVec

	// Delivery metrics
	DeliveryAttempts *prometheus.CounterVec
	DeliveryDuration *prometheus.HistogramVec

	// Callback metrics
	CallbackAttempts *prometheus.CounterVec
	CallbackDuration *prometheus.HistogramVec

	// HTTP metrics
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	// Circuit breaker metrics
	CircuitBreakerState       prometheus.Gauge
	CircuitBreakerTransitions *prometheus.CounterVec

	// IP reputation metrics
	IPReputationDegraded *prometheus.GaugeVec
	IPReputationEvents   *prometheus.CounterVec

	// Database metrics
	DatabaseSize          prometheus.Gauge
	DatabaseWALSize       prometheus.Gauge
	DatabaseConnections   prometheus.Gauge
	DatabaseQueryDuration *prometheus.HistogramVec
}

// NewMetrics creates and registers all Prometheus metrics with the default
// registry. This should be called once during application initialization.
// All metrics are automatically registered via promauto and will be exposed
// by prometheus/promhttp.Handler().
func NewMetrics() *Metrics {
	return &Metrics{
		// Queue depth by status (pending, delivering, failed, delivered)
		QueueDepth: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "fune_queue_depth",
				Help: "Number of messages in queue by status",
			},
			[]string{"status"},
		),

		// Delivery attempts by outcome (success, temporary_error, permanent_error, network_error, throttled)
		DeliveryAttempts: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fune_delivery_attempts_total",
				Help: "Total number of delivery attempts by outcome",
			},
			[]string{"outcome"},
		),

		// Delivery duration histogram
		DeliveryDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "fune_delivery_duration_seconds",
				Help:    "Time taken for delivery attempts",
				Buckets: []float64{.1, .5, 1, 2, 5, 10, 30, 60, 120},
			},
			[]string{"outcome"},
		),

		// Callback attempts by outcome (success, failure)
		CallbackAttempts: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fune_callback_attempts_total",
				Help: "Total number of webhook callback attempts by outcome",
			},
			[]string{"outcome", "event_type"},
		),

		// Callback duration histogram
		CallbackDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "fune_callback_duration_seconds",
				Help:    "Time taken for webhook callbacks",
				Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10},
			},
			[]string{"outcome"},
		),

		// HTTP requests by method and status code
		HTTPRequests: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fune_http_requests_total",
				Help: "Total number of HTTP requests by method and status code",
			},
			[]string{"method", "path", "status"},
		),

		// HTTP request duration histogram
		HTTPDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "fune_http_request_duration_seconds",
				Help:    "HTTP request latency",
				Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
			},
			[]string{"method", "path"},
		),

		// Circuit breaker state (0=closed, 1=half-open, 2=open)
		CircuitBreakerState: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "fune_circuit_breaker_state",
				Help: "Current circuit breaker state (0=closed, 1=half-open, 2=open)",
			},
		),

		// Circuit breaker state transitions
		CircuitBreakerTransitions: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fune_circuit_breaker_transitions_total",
				Help: "Total number of circuit breaker state transitions",
			},
			[]string{"from_state", "to_state"},
		),

		// IP reputation - number of degraded IPs by source IP
		IPReputationDegraded: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "fune_ip_reputation_degraded",
				Help: "IP reputation status (1=degraded, 0=healthy) by source IP",
			},
			[]string{"source_ip"},
		),

		// IP reputation events (degraded, recovered)
		IPReputationEvents: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fune_ip_reputation_events_total",
				Help: "Total number of IP reputation events by event type and source IP",
			},
			[]string{"event_type", "source_ip"},
		),

		// Database size in bytes
		DatabaseSize: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "fune_database_size_bytes",
				Help: "Size of the SQLite database file in bytes",
			},
		),

		// Database WAL size in bytes
		DatabaseWALSize: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "fune_database_wal_size_bytes",
				Help: "Size of the SQLite WAL file in bytes",
			},
		),

		// Active database connections
		DatabaseConnections: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "fune_database_connections",
				Help: "Number of active database connections",
			},
		),

		// Database query duration
		DatabaseQueryDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "fune_database_query_duration_seconds",
				Help:    "Time taken for database queries",
				Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
			},
			[]string{"operation"},
		),
	}
}

// RecordQueueDepth updates the queue depth gauge for a specific message status.
// Status should be one of: "queued", "sending", "delivered", "failed".
func (m *Metrics) RecordQueueDepth(status string, count int64) {
	m.QueueDepth.WithLabelValues(status).Set(float64(count))
}

// RecordDeliveryAttempt records a delivery attempt with its outcome and duration.
// Outcome should be one of: "success", "temporary_error", "permanent_error",
// "network_error", "throttled". Duration is in seconds.
func (m *Metrics) RecordDeliveryAttempt(outcome string, duration float64) {
	m.DeliveryAttempts.WithLabelValues(outcome).Inc()
	m.DeliveryDuration.WithLabelValues(outcome).Observe(duration)
}

// RecordCallbackAttempt records a webhook callback attempt with its outcome,
// event type, and duration. Outcome: "success" or "failure". EventType:
// "delivered", "hard_bounce", etc. Duration is in seconds.
func (m *Metrics) RecordCallbackAttempt(outcome, eventType string, duration float64) {
	m.CallbackAttempts.WithLabelValues(outcome, eventType).Inc()
	m.CallbackDuration.WithLabelValues(outcome).Observe(duration)
}

// RecordHTTPRequest records an HTTP request with method, path, status code,
// and duration. Status should be HTTP status code as string ("200", "404", etc.).
// Duration is in seconds.
func (m *Metrics) RecordHTTPRequest(method, path, status string, duration float64) {
	m.HTTPRequests.WithLabelValues(method, path, status).Inc()
	m.HTTPDuration.WithLabelValues(method, path).Observe(duration)
}

// SetCircuitBreakerState sets the current circuit breaker state as a gauge.
// State values: 0=closed (normal), 1=half-open (testing), 2=open (failing).
func (m *Metrics) SetCircuitBreakerState(state int) {
	m.CircuitBreakerState.Set(float64(state))
}

// RecordCircuitBreakerTransition records a circuit breaker state transition.
// States: "closed", "half_open", "open". Useful for alerting on state changes.
func (m *Metrics) RecordCircuitBreakerTransition(fromState, toState string) {
	m.CircuitBreakerTransitions.WithLabelValues(fromState, toState).Inc()
}

// SetIPReputationDegraded sets the degraded status for a source IP address.
// Sets gauge to 1.0 if degraded, 0.0 if healthy. Used for IP reputation tracking.
func (m *Metrics) SetIPReputationDegraded(sourceIP string, degraded bool) {
	value := 0.0
	if degraded {
		value = 1.0
	}
	m.IPReputationDegraded.WithLabelValues(sourceIP).Set(value)
}

// RecordIPReputationEvent records an IP reputation event for alerting.
// EventType: "degraded" (IP marked as bad) or "recovered" (IP restored to pool).
func (m *Metrics) RecordIPReputationEvent(eventType, sourceIP string) {
	m.IPReputationEvents.WithLabelValues(eventType, sourceIP).Inc()
}

// SetDatabaseSize sets the SQLite database file size in bytes.
// Useful for monitoring database growth and triggering maintenance.
func (m *Metrics) SetDatabaseSize(sizeBytes int64) {
	m.DatabaseSize.Set(float64(sizeBytes))
}

// SetDatabaseWALSize sets the SQLite Write-Ahead Log file size in bytes.
// Large WAL sizes may indicate checkpoint issues or high write load.
func (m *Metrics) SetDatabaseWALSize(sizeBytes int64) {
	m.DatabaseWALSize.Set(float64(sizeBytes))
}

// SetDatabaseConnections sets the number of active SQLite database connections.
// Useful for monitoring connection pool usage and detecting leaks.
func (m *Metrics) SetDatabaseConnections(count int) {
	m.DatabaseConnections.Set(float64(count))
}

// RecordDatabaseQuery records a database query execution time in seconds.
// Operation should describe the query type: "enqueue", "dequeue", "update", etc.
func (m *Metrics) RecordDatabaseQuery(operation string, duration float64) {
	m.DatabaseQueryDuration.WithLabelValues(operation).Observe(duration)
}
