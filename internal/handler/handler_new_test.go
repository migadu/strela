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

func getDefaultHTTPConfig() *config.HTTPConfig {
	cfg := &config.HTTPConfig{}
	// Apply defaults
	c := &config.Config{HTTP: *cfg}
	c.SetDefaults()
	return &c.HTTP
}

func TestQueueMessageHandler_Authentication(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	q, cleanup := queue.SetupTestQueue(t)
	defer cleanup()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultHTTPConfig()
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

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultHTTPConfig()
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

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := getDefaultHTTPConfig()
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

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	// Set a small limit for testing
	httpCfg := getDefaultHTTPConfig()
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
