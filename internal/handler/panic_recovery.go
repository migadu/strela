package handler

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// PanicRecoveryMiddleware wraps an HTTP handler with panic recovery to prevent
// server crashes. When a panic occurs, it logs the panic with full stack trace
// and returns a 500 Internal Server Error to the client.
//
// This is critical for production stability - without panic recovery, a single
// panic in any request handler will crash the entire server process.
func PanicRecoveryMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("HTTP handler panic recovered",
					"panic", rec,
					"path", r.URL.Path,
					"method", r.Method,
					"remote_addr", r.RemoteAddr,
					"stack", string(debug.Stack()))

				// Return 500 to client if response not already written
				// Note: if headers were already sent, this will have no effect
				http.Error(w, `{"error":"Internal Server Error"}`, http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}
