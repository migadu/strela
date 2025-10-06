package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"fune/internal/config"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func TestHandlerIdempotency_Disabled(t *testing.T) {
	// Create temporary database
	dbPath := "./test_handler_idem_disabled.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := &config.HTTPConfig{
		IdempotencyEnabled: false, // Disabled
		IdempotencyHeader:  "X-Idempotency-Key",
		MaxBodySizeBytes:   10 * 1024 * 1024, // 10MB
	}

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// Create request with idempotency key
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	body, _ := json.Marshal(requestBody)

	req := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", "test_key_001") // Should be ignored

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp EnqueueResponse
	json.NewDecoder(w.Body).Decode(&resp)
	messageID1 := resp.MessageID

	// Send same request again (create fresh body reader)
	body2, _ := json.Marshal(requestBody)
	req2 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Idempotency-Key", "test_key_001")

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var resp2 EnqueueResponse
	json.NewDecoder(w2.Body).Decode(&resp2)
	messageID2 := resp2.MessageID

	// Should create different messages (idempotency disabled)
	if messageID1 == messageID2 {
		t.Error("Should create different messages when idempotency is disabled")
	}
}

func TestHandlerIdempotency_Enabled_SameKey(t *testing.T) {
	// Create temporary database
	dbPath := "./test_handler_idem_enabled.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := &config.HTTPConfig{
		IdempotencyEnabled:  true, // Enabled
		IdempotencyHeader:   "X-Idempotency-Key",
		IdempotencyTTLHours: 24,
		MaxBodySizeBytes:    10 * 1024 * 1024, // 10MB
	}

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// First request
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	body, _ := json.Marshal(requestBody)

	req1 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Idempotency-Key", "test_key_same")

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("Expected 200 for first request, got %d", w1.Code)
	}

	var resp1 EnqueueResponse
	json.NewDecoder(w1.Body).Decode(&resp1)
	messageID1 := resp1.MessageID

	// Second request with same idempotency key (create fresh body reader)
	body2, _ := json.Marshal(requestBody)
	req2 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Idempotency-Key", "test_key_same")

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusAccepted {
		t.Fatalf("Expected 202 for duplicate request, got %d", w2.Code)
	}

	var resp2 EnqueueResponse
	json.NewDecoder(w2.Body).Decode(&resp2)
	messageID2 := resp2.MessageID

	// Should return same message_id (idempotent)
	if messageID1 != messageID2 {
		t.Errorf("Expected same message_id for duplicate request, got %s and %s", messageID1, messageID2)
	}

	// Verify only one message in queue
	messages, _ := q.GetNextMessages(10)
	if len(messages) != 1 {
		t.Errorf("Expected 1 message in queue, got %d", len(messages))
	}
}

func TestHandlerIdempotency_Enabled_DifferentKeys(t *testing.T) {
	// Create temporary database
	dbPath := "./test_handler_idem_diff_keys.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := &config.HTTPConfig{
		IdempotencyEnabled:  true,
		IdempotencyHeader:   "X-Idempotency-Key",
		IdempotencyTTLHours: 24,
		MaxBodySizeBytes:    10 * 1024 * 1024, // 10MB
	}

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// First request
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	body, _ := json.Marshal(requestBody)

	req1 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Idempotency-Key", "key_001")

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	var resp1 EnqueueResponse
	json.NewDecoder(w1.Body).Decode(&resp1)
	messageID1 := resp1.MessageID

	// Second request with different idempotency key (create fresh body reader)
	body2, _ := json.Marshal(requestBody)
	req2 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Idempotency-Key", "key_002")

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var resp2 EnqueueResponse
	json.NewDecoder(w2.Body).Decode(&resp2)
	messageID2 := resp2.MessageID

	// Should create different messages (different keys)
	if messageID1 == messageID2 {
		t.Error("Should create different messages for different idempotency keys")
	}

	// Verify two messages in queue
	messages, _ := q.GetNextMessages(10)
	if len(messages) != 2 {
		t.Errorf("Expected 2 messages in queue, got %d", len(messages))
	}
}

func TestHandlerIdempotency_CustomHeader(t *testing.T) {
	// Create temporary database
	dbPath := "./test_handler_custom_header.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := &config.HTTPConfig{
		IdempotencyEnabled:  true,
		IdempotencyHeader:   "X-Custom-Dedup-Key", // Custom header
		IdempotencyTTLHours: 24,
	}

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// Request with custom header
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	body, _ := json.Marshal(requestBody)

	req1 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Custom-Dedup-Key", "custom_key_001")

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	var resp1 EnqueueResponse
	json.NewDecoder(w1.Body).Decode(&resp1)
	messageID1 := resp1.MessageID

	// Retry with same custom header
	req2 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Custom-Dedup-Key", "custom_key_001")

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var resp2 EnqueueResponse
	json.NewDecoder(w2.Body).Decode(&resp2)
	messageID2 := resp2.MessageID

	// Should be idempotent with custom header
	if messageID1 != messageID2 {
		t.Errorf("Custom header should work for idempotency, got different message_ids")
	}
}

func TestHandlerIdempotency_AfterTerminalState(t *testing.T) {
	// Create temporary database
	dbPath := "./test_handler_terminal.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := queue.NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	deliveryCfg := &config.DeliveryConfig{
		MaxMessageAgeHours: 48,
	}

	httpCfg := &config.HTTPConfig{
		IdempotencyEnabled:  true,
		IdempotencyHeader:   "X-Idempotency-Key",
		IdempotencyTTLHours: 24,
	}

	handler := NewQueueMessageHandler(q, deliveryCfg, httpCfg, nil, logger)

	// First request
	requestBody := map[string]string{
		"from":    "sender@example.com",
		"to":      "recipient@example.com",
		"subject": "Test",
		"text":    "Test message",
	}
	body, _ := json.Marshal(requestBody)

	req1 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Idempotency-Key", "terminal_key")

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	var resp1 EnqueueResponse
	json.NewDecoder(w1.Body).Decode(&resp1)
	messageID := resp1.MessageID
	initialStatus := resp1.Status

	// Simulate message delivery (update to terminal state)
	q.UpdateStatus(messageID, queue.StatusDelivered)

	// Retry after terminal state
	req2 := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Idempotency-Key", "terminal_key")

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	var resp2 EnqueueResponse
	json.NewDecoder(w2.Body).Decode(&resp2)

	// Should return same message_id with updated status
	if resp2.MessageID != messageID {
		t.Errorf("Expected same message_id after terminal state, got %s", resp2.MessageID)
	}

	if resp2.Status == initialStatus {
		t.Logf("Note: Status may be updated to 'delivered' (expected behavior)")
	}
}
