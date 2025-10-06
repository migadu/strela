package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the application
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
}

// NewMetrics creates and registers all Prometheus metrics
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
	}
}

// RecordQueueDepth updates queue depth metrics
func (m *Metrics) RecordQueueDepth(status string, count int64) {
	m.QueueDepth.WithLabelValues(status).Set(float64(count))
}

// RecordDeliveryAttempt records a delivery attempt
func (m *Metrics) RecordDeliveryAttempt(outcome string, duration float64) {
	m.DeliveryAttempts.WithLabelValues(outcome).Inc()
	m.DeliveryDuration.WithLabelValues(outcome).Observe(duration)
}

// RecordCallbackAttempt records a callback attempt
func (m *Metrics) RecordCallbackAttempt(outcome, eventType string, duration float64) {
	m.CallbackAttempts.WithLabelValues(outcome, eventType).Inc()
	m.CallbackDuration.WithLabelValues(outcome).Observe(duration)
}

// RecordHTTPRequest records an HTTP request
func (m *Metrics) RecordHTTPRequest(method, path, status string, duration float64) {
	m.HTTPRequests.WithLabelValues(method, path, status).Inc()
	m.HTTPDuration.WithLabelValues(method, path).Observe(duration)
}

// SetCircuitBreakerState sets the current circuit breaker state
func (m *Metrics) SetCircuitBreakerState(state int) {
	m.CircuitBreakerState.Set(float64(state))
}

// RecordCircuitBreakerTransition records a state transition
func (m *Metrics) RecordCircuitBreakerTransition(fromState, toState string) {
	m.CircuitBreakerTransitions.WithLabelValues(fromState, toState).Inc()
}

// SetIPReputationDegraded sets the degraded status for a source IP
func (m *Metrics) SetIPReputationDegraded(sourceIP string, degraded bool) {
	value := 0.0
	if degraded {
		value = 1.0
	}
	m.IPReputationDegraded.WithLabelValues(sourceIP).Set(value)
}

// RecordIPReputationEvent records an IP reputation event (degraded or recovered)
func (m *Metrics) RecordIPReputationEvent(eventType, sourceIP string) {
	m.IPReputationEvents.WithLabelValues(eventType, sourceIP).Inc()
}
