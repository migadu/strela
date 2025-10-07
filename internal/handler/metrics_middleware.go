package handler

import (
	"net/http"
	"strconv"
	"time"
)

// HTTPMetrics defines the interface for recording HTTP request metrics.
//
// Implementations typically export metrics to Prometheus, Datadog, or other monitoring
// systems. The interface is designed to be simple and focused on HTTP-specific metrics.
//
// Example Implementation:
//
//	type PrometheusMetrics struct {
//	    requestDuration *prometheus.HistogramVec
//	    requestCount    *prometheus.CounterVec
//	}
//
//	func (m *PrometheusMetrics) RecordHTTPRequest(method, path, status string, duration float64) {
//	    m.requestDuration.WithLabelValues(method, path, status).Observe(duration)
//	    m.requestCount.WithLabelValues(method, path, status).Inc()
//	}
type HTTPMetrics interface {
	// RecordHTTPRequest records a completed HTTP request with timing information.
	//
	// Parameters:
	//   - method: HTTP method (GET, POST, etc.)
	//   - path: Request path (/v1/messages, /health, etc.)
	//   - status: HTTP status code as string ("200", "404", "500", etc.)
	//   - duration: Request duration in seconds (use time.Since(start).Seconds())
	//
	// This method is called after each HTTP request completes, including failed requests.
	RecordHTTPRequest(method, path, status string, duration float64)
}

// responseWriter wraps http.ResponseWriter to capture the HTTP status code.
//
// The standard http.ResponseWriter doesn't expose the status code after writing,
// so this wrapper intercepts WriteHeader() calls to record the status before
// passing the call through to the underlying writer.
//
// The wrapper also tracks whether WriteHeader has been called explicitly, as Go's
// http package will implicitly call WriteHeader(200) on the first Write() call.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

// WriteHeader captures the status code and delegates to the underlying ResponseWriter.
//
// This method intercepts WriteHeader calls to record the status code for metrics.
// It only calls the underlying WriteHeader once, even if called multiple times,
// to comply with http.ResponseWriter contract (multiple calls are a no-op).
func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

// Write delegates to the underlying ResponseWriter and ensures status code is captured.
//
// If WriteHeader has not been explicitly called, this method calls WriteHeader(200)
// before writing, matching the behavior of http.ResponseWriter. This ensures we
// always capture a status code for metrics.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// MetricsMiddleware wraps an HTTP handler to record request metrics.
//
// This middleware captures HTTP request timing and status code information for all
// requests passing through it. Metrics are recorded after the request completes,
// including both successful and failed requests.
//
// Captured Metrics:
//   - HTTP method (GET, POST, etc.)
//   - Request path (/v1/messages, /health, etc.)
//   - HTTP status code (200, 400, 500, etc.)
//   - Request duration in seconds
//
// If metrics is nil, the middleware passes requests through without recording metrics.
// This allows disabling metrics collection without changing handler configuration.
//
// Example Usage:
//
//	metricsRecorder := metrics.NewMetrics()
//	mux := http.NewServeMux()
//	mux.Handle("/v1/messages", messageHandler)
//
//	// Wrap entire mux with metrics middleware
//	server := &http.Server{
//	    Handler: MetricsMiddleware(mux, metricsRecorder),
//	}
//
// Empty Paths:
//
// If the request path is empty, it defaults to "/" for metrics labeling purposes.
// This ensures all requests have a valid path label for metrics systems.
//
// Thread Safety:
//
// This middleware is safe for concurrent use by multiple goroutines. The underlying
// metrics recorder must also be thread-safe (e.g., Prometheus metrics).
func MetricsMiddleware(next http.Handler, metrics HTTPMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if metrics == nil {
			next.ServeHTTP(w, r)
			return
		}

		startTime := time.Now()

		// Wrap response writer to capture status code
		rw := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK, // Default status
		}

		// Call next handler
		next.ServeHTTP(rw, r)

		// Record metrics
		duration := time.Since(startTime).Seconds()
		statusStr := strconv.Itoa(rw.statusCode)
		path := r.URL.Path
		if path == "" {
			path = "/"
		}

		metrics.RecordHTTPRequest(r.Method, path, statusStr, duration)
	})
}
