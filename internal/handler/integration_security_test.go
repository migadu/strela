package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fune/internal/config"
	"fune/internal/queue"

	"log/slog"
)

// TestSecurityHeadersIntegration tests that security headers are applied to actual handler responses
func TestSecurityHeadersIntegration(t *testing.T) {
	logger := slog.Default()

	// Create test queue
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	// Create handler
	inboundCfg := &config.InboundConfig{
		AuthToken:        "test-token",
		MaxBodySizeBytes: 1024 * 1024,
	}
	outboundCfg := &config.OutboundConfig{}

	handler := NewQueueMessageHandler(q, outboundCfg, inboundCfg, nil, logger)

	// Wrap with security headers middleware
	secureHandler := SecurityHeadersMiddleware(handler)

	// Test various response scenarios
	tests := []struct {
		name           string
		method         string
		path           string
		authToken      string
		body           map[string]string
		expectedStatus int
	}{
		{
			name:           "Unauthorized request",
			method:         http.MethodPost,
			path:           "/",
			authToken:      "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Method not allowed",
			method:         http.MethodGet,
			path:           "/",
			authToken:      "Bearer test-token",
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:      "Valid request",
			method:    http.MethodPost,
			path:      "/",
			authToken: "Bearer test-token",
			body: map[string]string{
				"from":    "sender@example.com",
				"to":      "recipient@example.com",
				"subject": "Test",
				"text":    "Test message",
			},
			expectedStatus: http.StatusOK, // Handler returns 200, not 202
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			if tt.body != nil {
				body, _ = json.Marshal(tt.body)
			}

			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tt.authToken != "" {
				req.Header.Set("Authorization", tt.authToken)
			}

			rec := httptest.NewRecorder()
			secureHandler.ServeHTTP(rec, req)

			// Verify status code
			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rec.Code)
			}

			// Verify security headers are present in ALL responses
			securityHeaders := []string{
				"X-Content-Type-Options",
				"X-Frame-Options",
				"X-XSS-Protection",
				"Cache-Control",
				"Referrer-Policy",
				"Content-Security-Policy",
			}

			for _, header := range securityHeaders {
				if rec.Header().Get(header) == "" {
					t.Errorf("security header %s is missing from response", header)
				}
			}

			// Verify specific header values
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("X-Content-Type-Options should be 'nosniff'")
			}

			if rec.Header().Get("X-Frame-Options") != "DENY" {
				t.Error("X-Frame-Options should be 'DENY'")
			}
		})
	}
}
