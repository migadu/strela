// Package recovery provides panic recovery utilities for graceful error handling
// in goroutines and HTTP handlers. Prevents application crashes by catching panics,
// logging stack traces, and optionally executing recovery callbacks.
//
// Key Features:
//   - Panic recovery with full stack trace logging
//   - Safe goroutine wrapper with automatic panic handling
//   - Nested panic recovery (catches panics in panic handlers)
//   - HTTP middleware for panic recovery in request handlers
//   - Context-aware logging for identifying panic sources
//
// Usage Patterns:
//
// 1. Defer-based recovery in goroutines:
//
//	go func() {
//		defer recovery.RecoverPanic(logger, "worker-goroutine")
//		// ... work that might panic
//	}()
//
// 2. Wrapped goroutine launch:
//
//	recovery.SafeGo(logger, "background-task", func() {
//		// ... work that might panic
//	})
//
// 3. Recovery with custom callback:
//
//	defer recovery.RecoverPanicWithCallback(logger, "critical-section", func(r interface{}) {
//		// Custom cleanup or alerting logic
//		alerting.SendPanicAlert(r)
//	})
//
// Best Practices:
//
//   - Use defer recovery at the start of every goroutine
//   - Provide descriptive context strings for easy panic identification
//   - Avoid panics in production; use explicit error returns when possible
//   - Recovery is a safety net, not a primary error handling mechanism
package recovery

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// RecoverPanic recovers from panics and logs them with full stack traces.
// This should be called with defer at the start of every goroutine to prevent
// crashes from propagating and terminating the application.
//
// Example:
//
//	go func() {
//		defer recovery.RecoverPanic(logger, "worker-goroutine")
//		// ... goroutine work
//	}()
func RecoverPanic(logger *slog.Logger, context string) {
	if r := recover(); r != nil {
		logger.Error("panic recovered",
			"context", context,
			"panic", r,
			"stack", string(debug.Stack()))
	}
}

// SafeGo launches a goroutine with automatic panic recovery. This is a
// convenience wrapper that adds defer-based panic recovery to the function.
// Prefer this over raw "go" calls for background tasks.
//
// Example:
//
//	recovery.SafeGo(logger, "cleanup-task", func() {
//		// ... cleanup work that might panic
//	})
func SafeGo(logger *slog.Logger, context string, fn func()) {
	go func() {
		defer RecoverPanic(logger, context)
		fn()
	}()
}

// RecoverPanicWithCallback recovers from panics and executes a callback
// function with the panic value. The callback itself is protected by a nested
// panic handler to prevent cascading failures. Useful for cleanup or alerting.
//
// Example:
//
//	defer recovery.RecoverPanicWithCallback(logger, "critical-op", func(r interface{}) {
//		// Send alert or perform cleanup
//		alerting.SendPanicAlert(fmt.Sprintf("Critical panic: %v", r))
//	})
func RecoverPanicWithCallback(logger *slog.Logger, context string, onPanic func(interface{})) {
	if r := recover(); r != nil {
		logger.Error("panic recovered",
			"context", context,
			"panic", r,
			"stack", string(debug.Stack()))

		if onPanic != nil {
			// Call the panic handler in a safe way (catch panics in the handler too)
			func() {
				defer func() {
					if r2 := recover(); r2 != nil {
						logger.Error("panic in panic handler",
							"context", context,
							"secondary_panic", r2)
					}
				}()
				onPanic(r)
			}()
		}
	}
}

// HTTPPanicHandler returns an HTTP middleware function that recovers from panics
// in HTTP handlers. When a panic occurs, it logs the panic and stack trace, then
// re-panics to allow the HTTP server to return a 500 Internal Server Error.
//
// This is intentional: we want to log the panic but still signal to the HTTP
// server that the request failed. The HTTP server will handle the re-panic
// gracefully and return an appropriate error response to the client.
//
// Example:
//
//	middleware := recovery.HTTPPanicHandler(logger)
//	wrappedHandler := middleware(func() {
//		// ... HTTP handler logic that might panic
//	})
func HTTPPanicHandler(logger *slog.Logger) func(next func()) func() {
	return func(next func()) func() {
		return func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("HTTP handler panic recovered",
						"panic", r,
						"stack", string(debug.Stack()))

					// The panic is recovered, the handler will return 500 automatically
					// because the response wasn't written
					panic(fmt.Sprintf("HTTP handler panic: %v", r))
				}
			}()
			next()
		}
	}
}
