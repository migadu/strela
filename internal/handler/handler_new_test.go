package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fune/internal/config"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func getDefaultInboundConfig() *config.InboundConfig {
	cfg := &config.InboundConfig{}
	// Apply defaults
	c := &config.Config{Inbound: *cfg}
	c.SetDefaults()
	return &c.Inbound
}

func TestQueueMessageHandler_Authentication(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = "test-secret-token-123"

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	tests := []struct {
		name           string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "valid token",
			authHeader:     "Bearer test-secret-token-123",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid token",
			authHeader:     "Bearer wrong-token",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "missing auth header",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "token without Bearer prefix",
			authHeader:     "test-secret-token-123",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "empty bearer token",
			authHeader:     "Bearer ",
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create valid request body
			reqBody := map[string]string{
				"from":    "sender@example.com",
				"to":      "recipient@example.com",
				"subject": "Test",
				"text":    "Test message",
			}
			bodyBytes, _ := json.Marshal(reqBody)

			req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestQueueMessageHandler_NoAuth(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = "" // No auth

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	reqBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should succeed even without auth header when auth is disabled
	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d (no auth required), got %d", http.StatusOK, w.Code)
	}
}

func TestQueueMessageHandler_ValidRequest(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = "test-token"

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	reqBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test Subject",
		"text":    "Test message body",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	// Verify response contains message_id
	var response EnqueueResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.MessageID == "" {
		t.Error("Expected message_id in response")
	}

	if response.Status != "queued" {
		t.Errorf("Expected status 'queued', got '%s'", response.Status)
	}

	// Verify message was enqueued
	messages, err := q.GetNextMessages(1)
	if err != nil {
		t.Fatalf("Failed to get messages: %v", err)
	}

	if len(messages) != 1 {
		t.Errorf("Expected 1 message in queue, got %d", len(messages))
	}
}

func TestQueueMessageHandler_BodySizeLimit(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	// Set a small limit for testing
	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = "test-token"
	httpCfg.MaxBodySizeBytes = 100 // 100 bytes limit

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// Create a request body larger than the limit
	largeText := string(make([]byte, 200)) // 200 bytes
	reqBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    largeText,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should return 413 Request Entity Too Large or 400 Bad Request
	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 413 or 400 for oversized body, got %d", w.Code)
	}
}

func TestQueueMessageHandler_RateLimit(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = ""
	httpCfg.RateLimitEnabled = true
	httpCfg.RateLimitRequestsPerIP = 3
	httpCfg.RateLimitWindowSeconds = 10

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)
	defer handler.rateLimiter.Stop()

	reqBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// First 3 requests should succeed
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.168.1.100:12345"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: expected status %d, got %d", i+1, http.StatusOK, w.Code)
		}
	}

	// 4th request should be rate limited
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.100:12345"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("4th request: expected status %d, got %d", http.StatusTooManyRequests, w.Code)
	}

	// Verify error message
	var response map[string]string
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response["error"] != "API Rate Limit Exceeded" {
		t.Errorf("Expected error 'API Rate Limit Exceeded', got '%s'", response["error"])
	}
}

func TestQueueMessageHandler_RateLimit_DifferentIPs(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.OutboundConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultInboundConfig()
	httpCfg.AuthToken = ""
	httpCfg.RateLimitEnabled = true
	httpCfg.RateLimitRequestsPerIP = 2
	httpCfg.RateLimitWindowSeconds = 10

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)
	defer handler.rateLimiter.Stop()

	reqBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	// IP1: 2 requests should succeed
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.168.1.100:12345"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("IP1 request %d: expected status %d, got %d", i+1, http.StatusOK, w.Code)
		}
	}

	// IP2: 2 requests should also succeed (different IP)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.168.1.101:12345"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("IP2 request %d: expected status %d, got %d", i+1, http.StatusOK, w.Code)
		}
	}

	// IP1: 3rd request should be rate limited
	req1 := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req1.Header.Set("Content-Type", "application/json")
	req1.RemoteAddr = "192.168.1.100:12345"

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusTooManyRequests {
		t.Errorf("IP1 3rd request: expected status %d, got %d", http.StatusTooManyRequests, w1.Code)
	}

	// IP2: 3rd request should also be rate limited
	req2 := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyBytes))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "192.168.1.101:12345"

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("IP2 3rd request: expected status %d, got %d", http.StatusTooManyRequests, w2.Code)
	}
}
