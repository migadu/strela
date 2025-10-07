package handler

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter per IP address
type RateLimiter struct {
	mu              sync.RWMutex
	buckets         map[string]*bucket
	requestsPerIP   int
	windowSeconds   int
	cleanupInterval time.Duration
	stopCh          chan struct{}
}

// bucket represents a token bucket for a single IP address
type bucket struct {
	tokens     int
	lastRefill time.Time
}

// NewRateLimiter creates a new rate limiter
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

// Allow checks if a request from the given IP should be allowed
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

// cleanup removes old buckets to prevent memory leaks
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

// Stop stops the cleanup goroutine
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

// extractIP extracts the client IP address from the request
// Handles X-Forwarded-For and X-Real-IP headers for proxied requests
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
