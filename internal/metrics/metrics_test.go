package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNewMetrics(t *testing.T) {
	m := NewMetrics()

	if m == nil {
		t.Fatal("NewMetrics() returned nil")
	}

	// Verify all metrics are initialized
	if m.QueueDepth == nil {
		t.Error("QueueDepth is nil")
	}
	if m.DeliveryAttempts == nil {
		t.Error("DeliveryAttempts is nil")
	}
	if m.DeliveryDuration == nil {
		t.Error("DeliveryDuration is nil")
	}
	if m.CallbackAttempts == nil {
		t.Error("CallbackAttempts is nil")
	}
	if m.CallbackDuration == nil {
		t.Error("CallbackDuration is nil")
	}
	if m.HTTPRequests == nil {
		t.Error("HTTPRequests is nil")
	}
	if m.HTTPDuration == nil {
		t.Error("HTTPDuration is nil")
	}
	if m.CircuitBreakerState == nil {
		t.Error("CircuitBreakerState is nil")
	}
	if m.CircuitBreakerTransitions == nil {
		t.Error("CircuitBreakerTransitions is nil")
	}
	if m.IPReputationDegraded == nil {
		t.Error("IPReputationDegraded is nil")
	}
	if m.IPReputationEvents == nil {
		t.Error("IPReputationEvents is nil")
	}
}

func TestRecordQueueDepth(t *testing.T) {
	// Create a custom registry to avoid global state pollution
	reg := prometheus.NewRegistry()
	queueDepth := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_queue_depth",
			Help: "Test queue depth",
		},
		[]string{"status"},
	)
	reg.MustRegister(queueDepth)

	m := &Metrics{QueueDepth: queueDepth}

	// Record queue depths for different statuses
	m.RecordQueueDepth("queued", 10)
	m.RecordQueueDepth("sending", 5)
	m.RecordQueueDepth("delivered", 100)

	// Verify the values
	tests := []struct {
		status   string
		expected float64
	}{
		{"queued", 10.0},
		{"sending", 5.0},
		{"delivered", 100.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := queueDepth.WithLabelValues(tt.status).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Gauge.GetValue() != tt.expected {
			t.Errorf("QueueDepth(%s) = %v, want %v", tt.status, metric.Gauge.GetValue(), tt.expected)
		}
	}
}

func TestRecordDeliveryAttempt(t *testing.T) {
	reg := prometheus.NewRegistry()

	deliveryAttempts := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_delivery_attempts_total",
			Help: "Test delivery attempts",
		},
		[]string{"outcome"},
	)
	deliveryDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "test_delivery_duration_seconds",
			Help:    "Test delivery duration",
			Buckets: []float64{.1, .5, 1, 2, 5, 10, 30, 60, 120},
		},
		[]string{"outcome"},
	)

	reg.MustRegister(deliveryAttempts)
	reg.MustRegister(deliveryDuration)

	m := &Metrics{
		DeliveryAttempts: deliveryAttempts,
		DeliveryDuration: deliveryDuration,
	}

	// Record some delivery attempts
	m.RecordDeliveryAttempt("success", 1.5)
	m.RecordDeliveryAttempt("success", 2.3)
	m.RecordDeliveryAttempt("temporary_error", 0.5)
	m.RecordDeliveryAttempt("permanent_error", 0.1)

	// Verify counter values
	tests := []struct {
		outcome  string
		expected float64
	}{
		{"success", 2.0},
		{"temporary_error", 1.0},
		{"permanent_error", 1.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := deliveryAttempts.WithLabelValues(tt.outcome).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Counter.GetValue() != tt.expected {
			t.Errorf("DeliveryAttempts(%s) = %v, want %v", tt.outcome, metric.Counter.GetValue(), tt.expected)
		}
	}

	// Verify histogram has observations
	// For histograms, we need to collect all metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Find the delivery duration histogram
	var sampleCount uint64
	for _, mf := range metricFamilies {
		if mf.GetName() == "test_delivery_duration_seconds" {
			for _, m := range mf.GetMetric() {
				// Check if this is the "success" label
				for _, label := range m.GetLabel() {
					if label.GetName() == "outcome" && label.GetValue() == "success" {
						sampleCount = m.Histogram.GetSampleCount()
						break
					}
				}
			}
		}
	}

	if sampleCount != 2 {
		t.Errorf("DeliveryDuration(success) sample count = %v, want 2", sampleCount)
	}
}

func TestRecordCallbackAttempt(t *testing.T) {
	reg := prometheus.NewRegistry()

	callbackAttempts := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_callback_attempts_total",
			Help: "Test callback attempts",
		},
		[]string{"outcome", "event_type"},
	)
	callbackDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "test_callback_duration_seconds",
			Help:    "Test callback duration",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"outcome"},
	)

	reg.MustRegister(callbackAttempts)
	reg.MustRegister(callbackDuration)

	m := &Metrics{
		CallbackAttempts: callbackAttempts,
		CallbackDuration: callbackDuration,
	}

	// Record callback attempts
	m.RecordCallbackAttempt("success", "delivered", 0.5)
	m.RecordCallbackAttempt("success", "delivered", 0.3)
	m.RecordCallbackAttempt("failure", "hard_bounce", 1.0)

	// Verify counter values
	tests := []struct {
		outcome   string
		eventType string
		expected  float64
	}{
		{"success", "delivered", 2.0},
		{"failure", "hard_bounce", 1.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := callbackAttempts.WithLabelValues(tt.outcome, tt.eventType).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Counter.GetValue() != tt.expected {
			t.Errorf("CallbackAttempts(%s, %s) = %v, want %v", tt.outcome, tt.eventType, metric.Counter.GetValue(), tt.expected)
		}
	}

	// Verify histogram
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var sampleCount uint64
	for _, mf := range metricFamilies {
		if mf.GetName() == "test_callback_duration_seconds" {
			for _, m := range mf.GetMetric() {
				for _, label := range m.GetLabel() {
					if label.GetName() == "outcome" && label.GetValue() == "success" {
						sampleCount = m.Histogram.GetSampleCount()
						break
					}
				}
			}
		}
	}

	if sampleCount != 2 {
		t.Errorf("CallbackDuration(success) sample count = %v, want 2", sampleCount)
	}
}

func TestRecordHTTPRequest(t *testing.T) {
	reg := prometheus.NewRegistry()

	httpRequests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_http_requests_total",
			Help: "Test HTTP requests",
		},
		[]string{"method", "path", "status"},
	)
	httpDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "test_http_request_duration_seconds",
			Help:    "Test HTTP duration",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		},
		[]string{"method", "path"},
	)

	reg.MustRegister(httpRequests)
	reg.MustRegister(httpDuration)

	m := &Metrics{
		HTTPRequests: httpRequests,
		HTTPDuration: httpDuration,
	}

	// Record HTTP requests
	m.RecordHTTPRequest("POST", "/v1/messages", "200", 0.05)
	m.RecordHTTPRequest("POST", "/v1/messages", "200", 0.03)
	m.RecordHTTPRequest("POST", "/v1/messages", "429", 0.01)
	m.RecordHTTPRequest("GET", "/health", "200", 0.001)

	// Verify counter values
	tests := []struct {
		method   string
		path     string
		status   string
		expected float64
	}{
		{"POST", "/v1/messages", "200", 2.0},
		{"POST", "/v1/messages", "429", 1.0},
		{"GET", "/health", "200", 1.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := httpRequests.WithLabelValues(tt.method, tt.path, tt.status).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Counter.GetValue() != tt.expected {
			t.Errorf("HTTPRequests(%s, %s, %s) = %v, want %v", tt.method, tt.path, tt.status, metric.Counter.GetValue(), tt.expected)
		}
	}

	// Verify histogram
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var sampleCount uint64
	for _, mf := range metricFamilies {
		if mf.GetName() == "test_http_request_duration_seconds" {
			for _, m := range mf.GetMetric() {
				// Check for matching labels
				methodMatch := false
				pathMatch := false
				for _, label := range m.GetLabel() {
					if label.GetName() == "method" && label.GetValue() == "POST" {
						methodMatch = true
					}
					if label.GetName() == "path" && label.GetValue() == "/v1/messages" {
						pathMatch = true
					}
				}
				if methodMatch && pathMatch {
					sampleCount = m.Histogram.GetSampleCount()
					break
				}
			}
		}
	}

	if sampleCount != 3 {
		t.Errorf("HTTPDuration(POST, /v1/messages) sample count = %v, want 3", sampleCount)
	}
}

func TestSetCircuitBreakerState(t *testing.T) {
	reg := prometheus.NewRegistry()

	circuitBreakerState := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "test_circuit_breaker_state",
			Help: "Test circuit breaker state",
		},
	)

	reg.MustRegister(circuitBreakerState)

	m := &Metrics{CircuitBreakerState: circuitBreakerState}

	// Test different states
	states := []int{0, 1, 2} // closed, half-open, open

	for _, state := range states {
		m.SetCircuitBreakerState(state)

		metric := &dto.Metric{}
		if err := circuitBreakerState.Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Gauge.GetValue() != float64(state) {
			t.Errorf("CircuitBreakerState = %v, want %v", metric.Gauge.GetValue(), float64(state))
		}
	}
}

func TestRecordCircuitBreakerTransition(t *testing.T) {
	reg := prometheus.NewRegistry()

	circuitBreakerTransitions := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_circuit_breaker_transitions_total",
			Help: "Test circuit breaker transitions",
		},
		[]string{"from_state", "to_state"},
	)

	reg.MustRegister(circuitBreakerTransitions)

	m := &Metrics{CircuitBreakerTransitions: circuitBreakerTransitions}

	// Record transitions
	m.RecordCircuitBreakerTransition("closed", "open")
	m.RecordCircuitBreakerTransition("open", "half-open")
	m.RecordCircuitBreakerTransition("half-open", "closed")
	m.RecordCircuitBreakerTransition("half-open", "open")

	// Verify counter values
	tests := []struct {
		fromState string
		toState   string
		expected  float64
	}{
		{"closed", "open", 1.0},
		{"open", "half-open", 1.0},
		{"half-open", "closed", 1.0},
		{"half-open", "open", 1.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := circuitBreakerTransitions.WithLabelValues(tt.fromState, tt.toState).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Counter.GetValue() != tt.expected {
			t.Errorf("CircuitBreakerTransitions(%s, %s) = %v, want %v", tt.fromState, tt.toState, metric.Counter.GetValue(), tt.expected)
		}
	}
}

func TestSetIPReputationDegraded(t *testing.T) {
	reg := prometheus.NewRegistry()

	ipReputationDegraded := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_ip_reputation_degraded",
			Help: "Test IP reputation degraded",
		},
		[]string{"source_ip"},
	)

	reg.MustRegister(ipReputationDegraded)

	m := &Metrics{IPReputationDegraded: ipReputationDegraded}

	// Test setting degraded status
	tests := []struct {
		sourceIP string
		degraded bool
		expected float64
	}{
		{"192.168.1.100", true, 1.0},
		{"192.168.1.101", false, 0.0},
		{"2001:db8::1", true, 1.0},
	}

	for _, tt := range tests {
		m.SetIPReputationDegraded(tt.sourceIP, tt.degraded)

		metric := &dto.Metric{}
		if err := ipReputationDegraded.WithLabelValues(tt.sourceIP).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Gauge.GetValue() != tt.expected {
			t.Errorf("IPReputationDegraded(%s, %v) = %v, want %v", tt.sourceIP, tt.degraded, metric.Gauge.GetValue(), tt.expected)
		}
	}
}

func TestRecordIPReputationEvent(t *testing.T) {
	reg := prometheus.NewRegistry()

	ipReputationEvents := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_ip_reputation_events_total",
			Help: "Test IP reputation events",
		},
		[]string{"event_type", "source_ip"},
	)

	reg.MustRegister(ipReputationEvents)

	m := &Metrics{IPReputationEvents: ipReputationEvents}

	// Record events
	m.RecordIPReputationEvent("degraded", "192.168.1.100")
	m.RecordIPReputationEvent("recovered", "192.168.1.100")
	m.RecordIPReputationEvent("degraded", "192.168.1.101")

	// Verify counter values
	tests := []struct {
		eventType string
		sourceIP  string
		expected  float64
	}{
		{"degraded", "192.168.1.100", 1.0},
		{"recovered", "192.168.1.100", 1.0},
		{"degraded", "192.168.1.101", 1.0},
	}

	for _, tt := range tests {
		metric := &dto.Metric{}
		if err := ipReputationEvents.WithLabelValues(tt.eventType, tt.sourceIP).Write(metric); err != nil {
			t.Fatalf("Failed to write metric: %v", err)
		}
		if metric.Counter.GetValue() != tt.expected {
			t.Errorf("IPReputationEvents(%s, %s) = %v, want %v", tt.eventType, tt.sourceIP, metric.Counter.GetValue(), tt.expected)
		}
	}
}

func TestMetrics_MultipleUpdates(t *testing.T) {
	reg := prometheus.NewRegistry()

	queueDepth := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "test_queue_depth_updates",
			Help: "Test queue depth with multiple updates",
		},
		[]string{"status"},
	)

	reg.MustRegister(queueDepth)

	m := &Metrics{QueueDepth: queueDepth}

	// Update the same metric multiple times
	m.RecordQueueDepth("queued", 10)
	m.RecordQueueDepth("queued", 20)
	m.RecordQueueDepth("queued", 5)

	// The last value should be recorded (gauge behavior)
	metric := &dto.Metric{}
	if err := queueDepth.WithLabelValues("queued").Write(metric); err != nil {
		t.Fatalf("Failed to write metric: %v", err)
	}
	if metric.Gauge.GetValue() != 5.0 {
		t.Errorf("QueueDepth after multiple updates = %v, want 5.0", metric.Gauge.GetValue())
	}
}
