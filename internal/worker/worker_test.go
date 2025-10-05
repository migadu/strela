package worker

import (
	"testing"
	"time"

	"fune/internal/callback"
	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/queue"

	"go.uber.org/zap"
)

func setupTestWorker(t *testing.T) (*Worker, *queue.Queue, func()) {
	t.Helper()

	logger, _ := zap.NewDevelopment()
	q, queueCleanup := queue.SetupTestQueue(t)

	cfg := &config.DeliveryConfig{
		SourceIPs:                 []string{"127.0.0.1"},
		IPSelection:               "round-robin",
		MXCacheTTLSeconds:         3600,
		ConnectionTimeoutSeconds:  5,
		SMTPTimeoutSeconds:        10,
		MaxMessageAgeHours:        48,
		InitialRetryDelaySeconds:  300,
		MaxRetryDelaySeconds:      43200,
		BackoffMultiplier:         2.0,
		GreylistRetryDelaySeconds: 120,
	}

	callbackConfig := &config.CallbacksConfig{
		WebhookURL:               "https://example.com/webhook",
		AuthToken:                "test-token",
		TimeoutSeconds:           10,
		MaxCallbackAgeHours:      48,
		InitialRetryDelaySeconds: 30,
		MaxRetryDelaySeconds:     3600,
		BackoffMultiplier:        2.0,
		BatchSize:                10,
	}

	queueCfg := &config.QueueConfig{
		BatchSize:              5,
		CleanupIntervalSeconds: 60,
		PollIntervalSeconds:    30,
	}

	mxLookup := delivery.NewMXLookup(q, cfg, logger)
	deliverer := delivery.NewDeliverer(cfg, mxLookup, logger)
	retryScheduler := delivery.NewRetryScheduler(cfg)
	callbackHandler := callback.NewCallbackHandler(q, callbackConfig, logger)

	worker := NewWorker(q, deliverer, retryScheduler, callbackHandler, cfg, queueCfg, logger)

	cleanup := func() {
		queueCleanup()
	}

	return worker, q, cleanup
}

func TestWorker_Creation(t *testing.T) {
	worker, _, cleanup := setupTestWorker(t)
	defer cleanup()

	if worker == nil {
		t.Fatal("Expected worker to be created")
	}

	if worker.queue == nil {
		t.Error("Worker should have queue")
	}

	if worker.deliverer == nil {
		t.Error("Worker should have deliverer")
	}

	if worker.retryScheduler == nil {
		t.Error("Worker should have retry scheduler")
	}

	if worker.callbackHandler == nil {
		t.Error("Worker should have callback handler")
	}
}

func TestWorker_HandleSuccess(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	// Create test message
	msg := &queue.QueuedMessage{
		MessageID:  "test_success",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   0,
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	q.Enqueue(msg)

	// Create success result
	result := &delivery.DeliveryResult{
		Success:      true,
		SMTPCode:     250,
		SMTPResponse: "OK",
		MXHost:       "mx1.example.com",
		SourceIP:     "127.0.0.1",
		DurationMs:   1000,
	}

	// Handle success
	worker.handleSuccess(msg, result)

	// Verify message was deleted
	retrieved, _ := q.GetMessage("test_success")
	if retrieved != nil {
		t.Error("Message should be deleted after successful delivery")
	}

	// Verify callback was enqueued
	callbacks, _ := q.GetPendingCallbacks(10)
	if len(callbacks) != 1 {
		t.Errorf("Expected 1 callback, got %d", len(callbacks))
	}

	if len(callbacks) > 0 && callbacks[0].EventType != "delivered" {
		t.Errorf("Expected 'delivered' event, got '%s'", callbacks[0].EventType)
	}
}

func TestWorker_HandlePermanentFailure(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	msg := &queue.QueuedMessage{
		MessageID:  "test_permanent",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   0,
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	q.Enqueue(msg)

	result := &delivery.DeliveryResult{
		Success:      false,
		SMTPCode:     550,
		SMTPResponse: "User not found",
		MXHost:       "mx1.example.com",
		SourceIP:     "127.0.0.1",
		Error: &delivery.DeliveryError{
			Category:     delivery.ErrorPermanent,
			SMTPCode:     550,
			SMTPResponse: "User not found",
			Message:      "User not found",
		},
	}

	worker.handlePermanentFailure(msg, result)

	// Verify message was deleted
	retrieved, _ := q.GetMessage("test_permanent")
	if retrieved != nil {
		t.Error("Message should be deleted after permanent failure")
	}

	// Verify hard bounce callback was enqueued
	callbacks, _ := q.GetPendingCallbacks(10)
	if len(callbacks) != 1 {
		t.Errorf("Expected 1 callback, got %d", len(callbacks))
	}

	if len(callbacks) > 0 && callbacks[0].EventType != "hard_bounce" {
		t.Errorf("Expected 'hard_bounce' event, got '%s'", callbacks[0].EventType)
	}
}

func TestWorker_HandleTemporaryFailure_Retry(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	msg := &queue.QueuedMessage{
		MessageID:  "test_temp",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   0,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	q.Enqueue(msg)

	result := &delivery.DeliveryResult{
		Success:      false,
		SMTPCode:     450,
		SMTPResponse: "Mailbox busy",
		Error: &delivery.DeliveryError{
			Category:     delivery.ErrorTemporary,
			SMTPCode:     450,
			SMTPResponse: "Mailbox busy",
			Message:      "Mailbox busy or unavailable",
		},
	}

	worker.handleTemporaryFailure(msg, result)

	// Verify message still exists with updated retry time
	retrieved, _ := q.GetMessage("test_temp")
	if retrieved == nil {
		t.Fatal("Message should still exist after temporary failure")
	}

	if retrieved.Attempts != 1 {
		t.Errorf("Expected 1 attempt, got %d", retrieved.Attempts)
	}

	if retrieved.Status != queue.StatusQueued {
		t.Errorf("Expected status queued, got %s", retrieved.Status)
	}

	// Verify retry is scheduled in the future
	if !retrieved.NextRetryAt.After(time.Now()) {
		t.Error("Next retry should be in the future")
	}
}

func TestWorker_HandleTemporaryFailure_Expired(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	// Create message that's already expired
	msg := &queue.QueuedMessage{
		MessageID:  "test_expired",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   5,
		CreatedAt:  time.Now().Add(-50 * time.Hour),
		ExpiresAt:  time.Now().Add(-2 * time.Hour), // Expired
	}

	q.Enqueue(msg)

	result := &delivery.DeliveryResult{
		Success: false,
		Error: &delivery.DeliveryError{
			Category: delivery.ErrorTemporary,
			Message:  "Temporary failure",
		},
	}

	worker.handleTemporaryFailure(msg, result)

	// Verify message was deleted
	retrieved, _ := q.GetMessage("test_expired")
	if retrieved != nil {
		t.Error("Expired message should be deleted")
	}

	// Verify temp_expired callback was enqueued
	callbacks, _ := q.GetPendingCallbacks(10)
	if len(callbacks) != 1 {
		t.Errorf("Expected 1 callback, got %d", len(callbacks))
	}

	if len(callbacks) > 0 && callbacks[0].EventType != "temp_expired" {
		t.Errorf("Expected 'temp_expired' event, got '%s'", callbacks[0].EventType)
	}
}

func TestWorker_HandleExpired(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	msg := &queue.QueuedMessage{
		MessageID:  "test_handle_expired",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   3,
		CreatedAt:  time.Now().Add(-50 * time.Hour),
		ExpiresAt:  time.Now().Add(-1 * time.Hour),
	}

	q.Enqueue(msg)

	worker.handleExpired(msg, nil)

	// Verify message was deleted
	retrieved, _ := q.GetMessage("test_handle_expired")
	if retrieved != nil {
		t.Error("Expired message should be deleted")
	}

	// Verify expired callback was enqueued
	callbacks, _ := q.GetPendingCallbacks(10)
	if len(callbacks) != 1 {
		t.Errorf("Expected 1 callback, got %d", len(callbacks))
	}

	if len(callbacks) > 0 && callbacks[0].EventType != "expired" {
		t.Errorf("Expected 'expired' event, got '%s'", callbacks[0].EventType)
	}
}

func TestWorker_PerformCleanup(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	// Create multiple expired messages
	for i := 0; i < 3; i++ {
		msg := &queue.QueuedMessage{
			MessageID:  "expired_" + string(rune('a'+i)),
			FromAddr:   "sender@example.com",
			ToAddr:     "recipient@example.com",
			ToDomain:   "example.com",
			Subject:    "Test",
			RawMessage: []byte("test"),
			CreatedAt:  time.Now().Add(-50 * time.Hour),
			ExpiresAt:  time.Now().Add(-1 * time.Hour),
		}
		q.Enqueue(msg)
	}

	// Perform cleanup
	worker.performCleanup()

	// Verify all expired messages were deleted
	for i := 0; i < 3; i++ {
		msgID := "expired_" + string(rune('a'+i))
		retrieved, _ := q.GetMessage(msgID)
		if retrieved != nil {
			t.Errorf("Message %s should be deleted after cleanup", msgID)
		}
	}

	// Verify callbacks were enqueued
	callbacks, _ := q.GetPendingCallbacks(10)
	if len(callbacks) != 3 {
		t.Errorf("Expected 3 callbacks, got %d", len(callbacks))
	}
}

func TestWorker_StartStop(t *testing.T) {
	worker, _, cleanup := setupTestWorker(t)
	defer cleanup()

	// Start workers
	worker.Start(2)

	// Give them time to start
	time.Sleep(100 * time.Millisecond)

	// Stop workers
	worker.Stop()

	// Should complete without hanging
}

func TestWorker_ProcessBatch_NoMessages(t *testing.T) {
	worker, _, cleanup := setupTestWorker(t)
	defer cleanup()

	// Process batch with no messages - should not panic
	worker.processBatch(0)
}

func TestWorker_ProcessBatch_WithMessages(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	// Create some test messages
	for i := 0; i < 3; i++ {
		msg := &queue.QueuedMessage{
			MessageID:  "batch_test_" + string(rune('a'+i)),
			FromAddr:   "sender@example.com",
			ToAddr:     "recipient@example.com",
			ToDomain:   "example.com",
			Subject:    "Test",
			RawMessage: []byte("test"),
			ExpiresAt:  time.Now().Add(48 * time.Hour),
		}
		q.Enqueue(msg)
	}

	// Note: processBatch will try to deliver these messages
	// They will fail because we're not running a real SMTP server
	// But we can verify the batch processing doesn't panic
	worker.processBatch(0)
}

func TestIsExpired_Function(t *testing.T) {
	// Test the standalone IsExpired function
	expiredMsg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}

	if !delivery.IsExpired(expiredMsg) {
		t.Error("Message should be expired")
	}

	validMsg := &queue.QueuedMessage{
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	if delivery.IsExpired(validMsg) {
		t.Error("Message should not be expired")
	}
}

func TestWorker_HandleFailure_NoError(t *testing.T) {
	worker, q, cleanup := setupTestWorker(t)
	defer cleanup()

	msg := &queue.QueuedMessage{
		MessageID:  "test_no_error",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	q.Enqueue(msg)

	// Result with no error - should handle gracefully
	result := &delivery.DeliveryResult{
		Success: false,
		Error:   nil, // No error
	}

	// Should not panic
	worker.handleFailure(msg, result)
}

func TestWorker_RecordAttemptError(t *testing.T) {
	_, q, cleanup := setupTestWorker(t)
	defer cleanup()

	msg := &queue.QueuedMessage{
		MessageID:  "test_record_attempt",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		Attempts:   0,
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	q.Enqueue(msg)

	// Create a failed delivery result
	result := &delivery.DeliveryResult{
		Success:      false,
		SMTPCode:     450,
		SMTPResponse: "Mailbox busy",
		MXHost:       "mx1.example.com",
		SourceIP:     "127.0.0.1",
		DurationMs:   500,
		Error: &delivery.DeliveryError{
			Category: delivery.ErrorTemporary,
			Message:  "Mailbox busy",
		},
	}

	// This would normally be called in processMessage
	attempt := &queue.DeliveryAttempt{
		MessageID:     msg.MessageID,
		AttemptNumber: 1,
		AttemptedAt:   time.Now(),
		MXHost:        result.MXHost,
		SourceIP:      result.SourceIP,
		SMTPCode:      result.SMTPCode,
		SMTPResponse:  result.SMTPResponse,
		Success:       result.Success,
		DurationMs:    result.DurationMs,
		Error:         result.Error.Message,
		ErrorCategory: string(result.Error.Category),
	}

	err := q.RecordAttempt(attempt)
	if err != nil {
		t.Errorf("Failed to record attempt: %v", err)
	}

	// Verify RecordAttempt succeeds - the function primarily logs attempts
	// The message's Attempts count is updated separately by the worker
	// This test verifies that recording delivery attempts doesn't error
}
