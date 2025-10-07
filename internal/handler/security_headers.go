package handler

import "net/http"

// SecurityHeadersMiddleware adds security headers to all HTTP responses
// These headers provide defense-in-depth even though Fune is designed as a trusted backend service
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Prevent clickjacking attacks
		w.Header().Set("X-Frame-Options", "DENY")

		// Enable XSS protection in older browsers
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		// Enforce HTTPS (only if TLS is enabled, can be overridden by load balancer)
		// Note: This is commented out as it should be handled by the load balancer/API gateway
		// w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		// Prevent caching of sensitive responses
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		w.Header().Set("Pragma", "no-cache")

		// Referrer policy - don't leak referrer information
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Content Security Policy - prevent inline scripts (defense in depth)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		// Call the next handler
		next.ServeHTTP(w, r)
	})
}
