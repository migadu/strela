package handler

import "net/http"

// SecurityHeadersMiddleware adds security headers to all HTTP responses.
//
// This middleware implements defense-in-depth security by adding standard HTTP
// security headers to protect against common web vulnerabilities. Although Fune
// is designed as a trusted backend service (not a public web application), these
// headers provide additional protection layers.
//
// Security Headers Applied:
//
//   - X-Content-Type-Options: nosniff
//     Prevents MIME type sniffing attacks by forcing browsers to respect Content-Type
//
//   - X-Frame-Options: DENY
//     Prevents clickjacking attacks by disallowing the page to be embedded in frames
//
//   - X-XSS-Protection: 1; mode=block
//     Enables XSS protection in older browsers (modern browsers use CSP instead)
//
//   - Cache-Control: no-store, no-cache, must-revalidate, private
//     Prevents caching of potentially sensitive API responses
//
//   - Pragma: no-cache
//     Legacy cache control for HTTP/1.0 compatibility
//
//   - Referrer-Policy: no-referrer
//     Prevents leaking referrer information to external sites
//
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
//     Strict CSP that disallows all content loading and frame embedding
//
// Note on HSTS:
//
// Strict-Transport-Security (HSTS) is intentionally not set by this middleware.
// HSTS should be configured at the load balancer or API gateway level to avoid
// issues with development environments and non-HTTPS deployments.
//
// Usage:
//
//	mux := http.NewServeMux()
//	mux.Handle("/v1/messages", messageHandler)
//	server := &http.Server{
//	    Handler: SecurityHeadersMiddleware(mux),
//	}
//
// Thread Safety:
//
// This middleware is safe for concurrent use by multiple goroutines.
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
