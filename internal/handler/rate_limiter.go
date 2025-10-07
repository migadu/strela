package handler

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter per client IP address.
//
// The rate limiter uses the token bucket algorithm to limit the number of requests
// per IP address within a sliding time window. Each IP address gets its own bucket
// with a configurable number of tokens that refill over time.
//
// Key Features:
//   - Per-IP rate limiting (extracts IP from X-Forwarded-For, X-Real-IP, or RemoteAddr)
//   - Token bucket algorithm with sliding window refills
//   - Automatic cleanup of inactive IP buckets to prevent memory leaks
//   - Thread-safe for concurrent request processing
//
// Algorithm:
//
//  1. Each IP starts with a full bucket of tokens (requestsPerIP)
//  2. Each request consumes one token
//  3. When the time window elapses, the bucket refills to maximum capacity
//  4. Requests are denied if no tokens are available
//
// Example:
//
//	With requestsPerIP=100 and windowSeconds=60:
//	- Client can make 100 requests in 60 seconds
//	- After 60 seconds, bucket refills to 100 tokens
//	- Requests beyond the limit are denied with 429 Too Many Requests
//
// Memory Management:
//
// The rate limiter automatically removes inactive IP buckets (those not used for 2x
// the window duration) to prevent unbounded memory growth. This cleanup runs in a
// background goroutine that must be stopped via Stop() when shutting down.
//
// Thread Safety:
//
// RateLimiter is safe for concurrent use by multiple goroutines. All public methods
// use mutex locking to protect shared state.
type RateLimiter struct {
	mu              sync.RWMutex
	buckets         map[string]*bucket
	requestsPerIP   int
	windowSeconds   int
	cleanupInterval time.Duration
	stopCh          chan struct{}
}

// bucket represents a token bucket for tracking rate limit state of a single IP address.
//
// Each bucket maintains:
//   - tokens: Current number of available request tokens
//   - lastRefill: Timestamp of the last token refill (used for window calculation)
//
// When the time elapsed since lastRefill exceeds the window duration, the bucket
// refills to maximum capacity. This implements a sliding window rate limit.
type bucket struct {
	tokens     int
	lastRefill time.Time
}

// NewRateLimiter creates a new per-IP rate limiter with the specified limits.
//
// Parameters:
//   - requestsPerIP: Maximum number of requests allowed per IP address in the time window
//   - windowSeconds: Time window in seconds for rate limiting (e.g., 60 for 1 minute)
//
// The rate limiter starts a background goroutine that periodically cleans up inactive
// IP buckets (those not used for 2x windowSeconds). The goroutine must be stopped by
// calling Stop() during shutdown to prevent goroutine leaks.
//
// Example:
//
//	// Allow 100 requests per IP per minute
//	rl := NewRateLimiter(100, 60)
//	defer rl.Stop() // Cleanup background goroutine
//
//	// Use in HTTP handler
//	if !rl.Allow(r) {
//	    http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
//	    return
//	}
func NewRateLimiter(requestsPerIP int, windowSeconds int) *RateLimiter {
	rl := &RateLimiter{
		buckets:         make(map[string]*bucket),
		requestsPerIP:   requestsPerIP,
		windowSeconds:   windowSeconds,
		cleanupInterval: time.Duration(windowSeconds*2) * time.Second,
		stopCh:          make(chan struct{}),
	}

	// Start cleanup goroutine to prevent memory leaks
	go rl.cleanup()

	return rl
}

// Allow checks if a request from the client IP should be allowed under the rate limit.
//
// This method extracts the client IP from the request (handling X-Forwarded-For and
// X-Real-IP headers for proxied requests), checks the token bucket for that IP, and
// consumes a token if available.
//
// Returns:
//   - true: Request is allowed (token consumed)
//   - false: Request exceeds rate limit (no tokens available)
//
// Behavior:
//   - New IPs get a full bucket of tokens minus one (for this request)
//   - Existing IPs have their tokens refilled if the time window has elapsed
//   - If no tokens are available, the request is denied
//   - If the IP cannot be extracted, the request is allowed (fail-open)
//
// The method is thread-safe and can be called concurrently from multiple goroutines.
//
// Example:
//
//	if !rateLimiter.Allow(r) {
//	    w.WriteHeader(http.StatusTooManyRequests)
//	    json.NewEncoder(w).Encode(map[string]string{
//	        "error": "API Rate Limit Exceeded",
//	    })
//	    return
//	}
func (rl *RateLimiter) Allow(r *http.Request) bool {
	ip := extractIP(r)
	if ip == "" {
		// If we can't extract IP, allow the request
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[ip]

	if !exists {
		// New IP - create bucket with full tokens minus one
		rl.buckets[ip] = &bucket{
			tokens:     rl.requestsPerIP - 1,
			lastRefill: now,
		}
		return true
	}

	// Refill tokens based on time elapsed
	elapsed := now.Sub(b.lastRefill)
	if elapsed >= time.Duration(rl.windowSeconds)*time.Second {
		// Window has passed - refill to max
		b.tokens = rl.requestsPerIP
		b.lastRefill = now
	}

	// Check if we have tokens available
	if b.tokens > 0 {
		b.tokens--
		return true
	}

	return false
}

// cleanup removes old buckets to prevent memory leaks from inactive IP addresses.
//
// This method runs in a background goroutine and periodically removes buckets that
// haven't been accessed in 2x the rate limit window duration. This prevents unbounded
// memory growth as new IP addresses make requests.
//
// The cleanup interval is 2x windowSeconds. For example, with a 60-second window,
// cleanup runs every 120 seconds and removes buckets older than 120 seconds.
//
// The goroutine stops when Stop() is called, which closes the stopCh channel.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.buckets {
				// Remove buckets that haven't been used in 2x the window duration
				if now.Sub(b.lastRefill) > rl.cleanupInterval {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		case <-rl.stopCh:
			return
		}
	}
}

// Stop stops the background cleanup goroutine and releases resources.
//
// This method must be called during shutdown to prevent goroutine leaks. After calling
// Stop(), the rate limiter can still be used for rate limiting checks, but old buckets
// will no longer be automatically cleaned up.
//
// Stop is safe to call multiple times (subsequent calls are no-ops after the first).
//
// Example:
//
//	rl := NewRateLimiter(100, 60)
//	defer rl.Stop() // Ensure cleanup goroutine stops on exit
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

// extractIP extracts the client IP address from an HTTP request.
//
// This function checks multiple sources in order of preference:
//  1. X-Forwarded-For header (takes the first IP in comma-separated list)
//  2. X-Real-IP header (single IP from trusted proxy)
//  3. RemoteAddr field (direct connection IP:port)
//
// X-Forwarded-For and X-Real-IP headers are commonly set by reverse proxies, load
// balancers, and CDNs to preserve the original client IP address.
//
// Returns:
//   - Client IP address as a string (without port)
//   - Empty string if IP cannot be parsed (caller should fail-open)
//
// Security Note:
//
// In production deployments, ensure your reverse proxy/load balancer is configured
// to strip and overwrite these headers from client requests. Otherwise, clients can
// spoof IP addresses and bypass rate limiting.
//
// Example header values:
//   - X-Forwarded-For: "203.0.113.1, 198.51.100.1" → returns "203.0.113.1"
//   - X-Real-IP: "203.0.113.1" → returns "203.0.113.1"
//   - RemoteAddr: "203.0.113.1:54321" → returns "203.0.113.1"
func extractIP(r *http.Request) string {
	// Check X-Forwarded-For header (comma-separated list, first is original client)
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// Take the first IP in the list
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' || xff[i] == ' ' {
				xff = xff[:i]
				break
			}
		}
		if ip := net.ParseIP(xff); ip != nil {
			return xff
		}
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return xri
		}
	}

	// Fall back to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
