package handler

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRateLimiter_Allow(t *testing.T) {
	rl := NewRateLimiter(3, 10, slog.New(slog.NewTextHandler(os.Stdout, nil))) // 3 requests per 10 seconds
	defer rl.Stop()

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "192.168.1.100:12345"

	// First 3 requests should be allowed
	for i := 0; i < 3; i++ {
		if !rl.Allow(req) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	// 4th request should be denied
	if rl.Allow(req) {
		t.Error("4th request should be denied")
	}

	// 5th request should also be denied
	if rl.Allow(req) {
		t.Error("5th request should be denied")
	}
}

func TestRateLimiter_WindowRefill(t *testing.T) {
	rl := NewRateLimiter(2, 1, slog.New(slog.NewTextHandler(os.Stdout, nil))) // 2 requests per 1 second
	defer rl.Stop()

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "192.168.1.100:12345"

	// Use up the tokens
	if !rl.Allow(req) {
		t.Error("first request should be allowed")
	}
	if !rl.Allow(req) {
		t.Error("second request should be allowed")
	}

	// 3rd request should be denied
	if rl.Allow(req) {
		t.Error("third request should be denied")
	}

	// Wait for window to expire
	time.Sleep(1100 * time.Millisecond)

	// Should be refilled now
	if !rl.Allow(req) {
		t.Error("request after window should be allowed")
	}
	if !rl.Allow(req) {
		t.Error("second request after window should be allowed")
	}
}

func TestRateLimiter_DifferentIPs(t *testing.T) {
	rl := NewRateLimiter(2, 10, slog.New(slog.NewTextHandler(os.Stdout, nil))) // 2 requests per 10 seconds
	defer rl.Stop()

	req1 := httptest.NewRequest("POST", "/v1/messages", nil)
	req1.RemoteAddr = "192.168.1.100:12345"

	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	req2.RemoteAddr = "192.168.1.101:12345"

	// Each IP should have its own bucket
	for i := 0; i < 2; i++ {
		if !rl.Allow(req1) {
			t.Errorf("req1 attempt %d should be allowed", i+1)
		}
		if !rl.Allow(req2) {
			t.Errorf("req2 attempt %d should be allowed", i+1)
		}
	}

	// Both should now be rate limited
	if rl.Allow(req1) {
		t.Error("req1 should be rate limited")
	}
	if rl.Allow(req2) {
		t.Error("req2 should be rate limited")
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name          string
		remoteAddr    string
		xForwardedFor string
		xRealIP       string
		expectedIP    string
	}{
		{
			name:       "RemoteAddr only",
			remoteAddr: "192.168.1.100:12345",
			expectedIP: "192.168.1.100",
		},
		{
			name:          "X-Forwarded-For single",
			remoteAddr:    "10.0.0.1:12345",
			xForwardedFor: "203.0.113.45",
			expectedIP:    "203.0.113.45",
		},
		{
			name:          "X-Forwarded-For multiple",
			remoteAddr:    "10.0.0.1:12345",
			xForwardedFor: "203.0.113.45, 10.0.0.2, 10.0.0.3",
			expectedIP:    "203.0.113.45",
		},
		{
			name:       "X-Real-IP",
			remoteAddr: "10.0.0.1:12345",
			xRealIP:    "203.0.113.45",
			expectedIP: "203.0.113.45",
		},
		{
			name:          "X-Forwarded-For takes precedence over X-Real-IP",
			remoteAddr:    "10.0.0.1:12345",
			xForwardedFor: "203.0.113.45",
			xRealIP:       "203.0.113.46",
			expectedIP:    "203.0.113.45",
		},
		{
			name:       "IPv6",
			remoteAddr: "[2001:db8::1]:12345",
			expectedIP: "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xForwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tt.xForwardedFor)
			}
			if tt.xRealIP != "" {
				req.Header.Set("X-Real-IP", tt.xRealIP)
			}

			got := extractIP(req)
			if got != tt.expectedIP {
				t.Errorf("extractIP() = %v, want %v", got, tt.expectedIP)
			}
		})
	}
}

func TestRateLimiter_Cleanup(t *testing.T) {
	// Use 1 second window, cleanup runs every 2 seconds by default
	rl := NewRateLimiter(10, 1, slog.New(slog.NewTextHandler(os.Stdout, nil))) // 10 requests per 1 second
	defer rl.Stop()

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "192.168.1.100:12345"

	// Make a request to create a bucket
	rl.Allow(req)

	// Verify bucket exists
	rl.mu.RLock()
	initialBuckets := len(rl.buckets)
	rl.mu.RUnlock()

	if initialBuckets != 1 {
		t.Errorf("expected 1 bucket, got %d", initialBuckets)
	}

	// Wait for window to expire (1s) + cleanup to run (2s) + margin
	time.Sleep(3500 * time.Millisecond)

	// Bucket should be cleaned up (expired buckets are removed)
	rl.mu.RLock()
	finalBuckets := len(rl.buckets)
	rl.mu.RUnlock()

	if finalBuckets != 0 {
		t.Errorf("expected 0 buckets after cleanup, got %d", finalBuckets)
	}
}

func TestRateLimiter_XForwardedForWithSpaces(t *testing.T) {
	rl := NewRateLimiter(2, 10, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	defer rl.Stop()

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.45 , 10.0.0.2")

	// Should extract first IP even with spaces
	if !rl.Allow(req) {
		t.Error("first request should be allowed")
	}
	if !rl.Allow(req) {
		t.Error("second request should be allowed")
	}

	// Should be rate limited
	if rl.Allow(req) {
		t.Error("third request should be denied")
	}
}
