package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the application.
type Metrics struct {
	// Delivery metrics
	DeliveryAttempts *prometheus.CounterVec
	DeliveryDuration *prometheus.HistogramVec
	ActiveDeliveries prometheus.Gauge

	// HTTP metrics
	HTTPRequests                 *prometheus.CounterVec
	HTTPDuration                 *prometheus.HistogramVec
	HTTPRequestsRejectedCapacity prometheus.Counter

	// IP reputation metrics
	IPReputationDegraded *prometheus.GaugeVec
	IPReputationEvents   *prometheus.CounterVec
}

// NewMetrics creates and registers all Prometheus metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		// Delivery attempts by outcome
		DeliveryAttempts: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "strela_delivery_attempts_total",
				Help: "Total number of delivery attempts by outcome",
			},
			[]string{"outcome"},
		),

		// Delivery duration histogram
		DeliveryDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "strela_delivery_duration_seconds",
				Help:    "Time taken for delivery attempts",
				Buckets: []float64{.1, .5, 1, 2, 5, 10, 30, 60, 120},
			},
			[]string{"outcome"},
		),

		// Active deliveries
		ActiveDeliveries: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "strela_active_deliveries",
				Help: "Number of active SMTP deliveries",
			},
		),

		// HTTP requests by method and status code
		HTTPRequests: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "strela_http_requests_total",
				Help: "Total number of HTTP requests by method and status code",
			},
			[]string{"method", "path", "status"},
		),

		// HTTP request duration histogram
		HTTPDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "strela_http_request_duration_seconds",
				Help:    "HTTP request latency",
				Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
			},
			[]string{"method", "path"},
		),

		// HTTP requests rejected due to capacity (concurrency limit)
		HTTPRequestsRejectedCapacity: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "strela_http_requests_rejected_capacity_total",
				Help: "Total HTTP requests rejected due to concurrency limit",
			},
		),

		// IP reputation - number of degraded IPs by source IP
		IPReputationDegraded: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "strela_ip_reputation_degraded",
				Help: "IP reputation status (1=degraded, 0=healthy) by source IP",
			},
			[]string{"source_ip"},
		),

		// IP reputation events (degraded, recovered)
		IPReputationEvents: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "strela_ip_reputation_events_total",
				Help: "Total number of IP reputation events by event type and source IP",
			},
			[]string{"event_type", "source_ip"},
		),
	}
}

// RecordDeliveryAttempt records a delivery attempt with its outcome and duration.
func (m *Metrics) RecordDeliveryAttempt(outcome string, duration float64) {
	m.DeliveryAttempts.WithLabelValues(outcome).Inc()
	m.DeliveryDuration.WithLabelValues(outcome).Observe(duration)
}

// RecordHTTPRequest records an HTTP request with method, path, status code, and duration.
func (m *Metrics) RecordHTTPRequest(method, path, status string, duration float64) {
	m.HTTPRequests.WithLabelValues(method, path, status).Inc()
	m.HTTPDuration.WithLabelValues(method, path).Observe(duration)
}

// RecordRejectedCapacity increments the rejected capacity counter.
func (m *Metrics) RecordRejectedCapacity() {
	m.HTTPRequestsRejectedCapacity.Inc()
}

// SetIPReputationDegraded sets the degraded status for a source IP address.
func (m *Metrics) SetIPReputationDegraded(sourceIP string, degraded bool) {
	value := 0.0
	if degraded {
		value = 1.0
	}
	m.IPReputationDegraded.WithLabelValues(sourceIP).Set(value)
}

// RecordIPReputationEvent records an IP reputation event.
func (m *Metrics) RecordIPReputationEvent(eventType, sourceIP string) {
	m.IPReputationEvents.WithLabelValues(eventType, sourceIP).Inc()
}
