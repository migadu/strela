package handler

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"strela/internal/recovery"
)

// RateLimiter implements a token bucket rate limiter per client IP address.
type RateLimiter struct {
	mu              sync.RWMutex
	buckets         map[string]*bucket
	requestsPerIP   int
	windowSeconds   int
	cleanupInterval time.Duration
	stopCh          chan struct{}
	logger          *slog.Logger
}

type bucket struct {
	tokens     int
	lastRefill time.Time
}

// NewRateLimiter creates a new per-IP rate limiter.
func NewRateLimiter(requestsPerIP int, windowSeconds int, logger *slog.Logger) *RateLimiter {
	if logger == nil {
		logger = slog.Default()
	}

	rl := &RateLimiter{
		buckets:         make(map[string]*bucket),
		requestsPerIP:   requestsPerIP,
		windowSeconds:   windowSeconds,
		cleanupInterval: time.Duration(windowSeconds*2) * time.Second,
		stopCh:          make(chan struct{}),
		logger:          logger,
	}

	// Start cleanup goroutine with panic recovery
	recovery.SafeGo(logger, "rate-limiter-cleanup", rl.cleanup)

	return rl
}

// Allow checks if a request from the client IP should be allowed.
func (rl *RateLimiter) Allow(r *http.Request) bool {
	ip := extractIP(r)
	if ip == "" {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[ip]

	if !exists {
		rl.buckets[ip] = &bucket{
			tokens:     rl.requestsPerIP - 1,
			lastRefill: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastRefill)
	if elapsed >= time.Duration(rl.windowSeconds)*time.Second {
		b.tokens = rl.requestsPerIP
		b.lastRefill = now
	}

	if b.tokens > 0 {
		b.tokens--
		return true
	}

	return false
}

// Middleware wraps an http.Handler with rate limiting.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "API Rate Limit Exceeded",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, b := range rl.buckets {
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

func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

func extractIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
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

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return xri
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
