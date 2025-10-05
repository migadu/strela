package handler

import (
	"net/http"
	"strconv"
	"time"
)

// HTTPMetrics interface for recording HTTP metrics
type HTTPMetrics interface {
	RecordHTTPRequest(method, path, status string, duration float64)
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// MetricsMiddleware wraps an HTTP handler to record metrics
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
