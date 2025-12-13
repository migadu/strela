package handler

import (
	"net/http"
)

// ConcurrencyLimitMiddleware limits the number of concurrent HTTP requests
func ConcurrencyLimitMiddleware(maxConcurrent int) func(http.Handler) http.Handler {
	if maxConcurrent <= 0 {
		// Unlimited concurrency
		return func(next http.Handler) http.Handler {
			return next
		}
	}

	// Buffered channel as semaphore
	sem := make(chan struct{}, maxConcurrent)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}: // Acquire slot
				defer func() { <-sem }() // Release slot
				next.ServeHTTP(w, r)

			default: // Semaphore full - reject immediately
				http.Error(w,
					`{"error":"Server at capacity, try again later"}`,
					http.StatusServiceUnavailable,
				)
			}
		})
	}
}
