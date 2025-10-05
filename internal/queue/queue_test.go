package queue

import (
	"testing"
	"time"
)

func TestQueue_Enqueue(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	msg := &QueuedMessage{
		MessageID:  "msg_test123",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test Subject",
		RawMessage: []byte("From: sender@example.com\r\nTo: recipient@example.com\r\n\r\nTest body"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	err := queue.Enqueue(msg)
	if err != nil {
		t.Fatalf("Failed to enqueue message: %v", err)
	}

	// Verify message was stored
	retrieved, err := queue.GetMessage("msg_test123")
	if err != nil {
		t.Fatalf("Failed to get message: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Message not found")
	}

	if retrieved.MessageID != msg.MessageID {
		t.Errorf("Expected message_id %s, got %s", msg.MessageID, retrieved.MessageID)
	}

	if retrieved.Status != StatusQueued {
		t.Errorf("Expected status %s, got %s", StatusQueued, retrieved.Status)
	}

	if retrieved.Attempts != 0 {
		t.Errorf("Expected 0 attempts, got %d", retrieved.Attempts)
	}
}

func TestQueue_GetNextMessages(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	// Enqueue multiple messages
	now := time.Now()

	msgs := []*QueuedMessage{
		{
			MessageID:  "msg_1",
			FromAddr:   "sender@example.com",
			ToAddr:     "recipient1@example.com",
			ToDomain:   "example.com",
			Subject:    "Test 1",
			RawMessage: []byte("test1"),
			ExpiresAt:  now.Add(48 * time.Hour),
		},
		{
			MessageID:  "msg_2",
			FromAddr:   "sender@example.com",
			ToAddr:     "recipient2@example.com",
			ToDomain:   "example.com",
			Subject:    "Test 2",
			RawMessage: []byte("test2"),
			ExpiresAt:  now.Add(48 * time.Hour),
		},
	}

	for _, msg := range msgs {
		if err := queue.Enqueue(msg); err != nil {
			t.Fatalf("Failed to enqueue: %v", err)
		}
	}

	// Get next messages
	retrieved, err := queue.GetNextMessages(10)
	if err != nil {
		t.Fatalf("Failed to get next messages: %v", err)
	}

	if len(retrieved) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(retrieved))
	}
}

func TestQueue_ScheduleRetry(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	msg := &QueuedMessage{
		MessageID:  "msg_retry",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	err := queue.Enqueue(msg)
	if err != nil {
		t.Fatalf("Failed to enqueue: %v", err)
	}

	// Schedule retry in the future
	nextRetry := time.Now().Add(5 * time.Minute)
	err = queue.ScheduleRetry("msg_retry", nextRetry, 1, "Test error", 450, "4.5.0 Mailbox busy")
	if err != nil {
		t.Fatalf("Failed to schedule retry: %v", err)
	}

	// Verify message is not returned immediately
	retrieved, err := queue.GetNextMessages(10)
	if err != nil {
		t.Fatalf("Failed to get next messages: %v", err)
	}

	if len(retrieved) != 0 {
		t.Errorf("Expected 0 messages ready now, got %d", len(retrieved))
	}

	// Verify message details were updated
	updated, err := queue.GetMessage("msg_retry")
	if err != nil {
		t.Fatalf("Failed to get message: %v", err)
	}

	if updated.Attempts != 1 {
		t.Errorf("Expected 1 attempt, got %d", updated.Attempts)
	}

	if updated.LastSMTPCode != 450 {
		t.Errorf("Expected SMTP code 450, got %d", updated.LastSMTPCode)
	}
}

func TestQueue_UpdateStatus(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	msg := &QueuedMessage{
		MessageID:  "msg_status",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	queue.Enqueue(msg)

	// Update status to delivered
	err := queue.UpdateStatus("msg_status", StatusDelivered)
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Verify status was updated
	updated, err := queue.GetMessage("msg_status")
	if err != nil {
		t.Fatalf("Failed to get message: %v", err)
	}

	if updated.Status != StatusDelivered {
		t.Errorf("Expected status %s, got %s", StatusDelivered, updated.Status)
	}
}

func TestQueue_RecordAttempt(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	msg := &QueuedMessage{
		MessageID:  "msg_attempt",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Test",
		RawMessage: []byte("test"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	queue.Enqueue(msg)

	attempt := &DeliveryAttempt{
		MessageID:     "msg_attempt",
		AttemptNumber: 1,
		AttemptedAt:   time.Now(),
		MXHost:        "mx1.example.com",
		SourceIP:      "192.168.1.100",
		SMTPCode:      250,
		SMTPResponse:  "OK",
		Success:       true,
		DurationMs:    1500,
		ErrorCategory: "",
	}

	err := queue.RecordAttempt(attempt)
	if err != nil {
		t.Fatalf("Failed to record attempt: %v", err)
	}

	// Verify attempt was recorded (check via raw SQL since we don't have a getter)
	var count int
	err = queue.db.QueryRow("SELECT COUNT(*) FROM delivery_attempts WHERE message_id = ?", "msg_attempt").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query attempts: %v", err)
	}

	if count != 1 {
		t.Errorf("Expected 1 attempt recorded, got %d", count)
	}
}

func TestQueue_FindExpiredMessages(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	now := time.Now()

	// Enqueue expired message
	expiredMsg := &QueuedMessage{
		MessageID:  "msg_expired",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Expired",
		RawMessage: []byte("test"),
		ExpiresAt:  now.Add(-1 * time.Hour), // Expired 1 hour ago
	}

	queue.Enqueue(expiredMsg)

	// Enqueue valid message
	validMsg := &QueuedMessage{
		MessageID:  "msg_valid",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Valid",
		RawMessage: []byte("test"),
		ExpiresAt:  now.Add(48 * time.Hour),
	}

	queue.Enqueue(validMsg)

	// Find expired messages
	expired, err := queue.FindExpiredMessages()
	if err != nil {
		t.Fatalf("Failed to find expired messages: %v", err)
	}

	if len(expired) != 1 {
		t.Errorf("Expected 1 expired message, got %d", len(expired))
	}

	if len(expired) > 0 && expired[0].MessageID != "msg_expired" {
		t.Errorf("Expected msg_expired, got %s", expired[0].MessageID)
	}
}

func TestQueue_DeleteMessage(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	msg := &QueuedMessage{
		MessageID:  "msg_delete",
		FromAddr:   "sender@example.com",
		ToAddr:     "recipient@example.com",
		ToDomain:   "example.com",
		Subject:    "Delete me",
		RawMessage: []byte("test"),
		ExpiresAt:  time.Now().Add(48 * time.Hour),
	}

	queue.Enqueue(msg)

	// Verify message exists
	retrieved, _ := queue.GetMessage("msg_delete")
	if retrieved == nil {
		t.Fatal("Message should exist before deletion")
	}

	// Delete message
	err := queue.DeleteMessage("msg_delete")
	if err != nil {
		t.Fatalf("Failed to delete message: %v", err)
	}

	// Verify message was deleted
	retrieved, _ = queue.GetMessage("msg_delete")
	if retrieved != nil {
		t.Error("Message should be deleted")
	}
}

func TestQueue_CallbackQueue(t *testing.T) {
	queue, cleanup := SetupTestQueue(t)
	defer cleanup()

	// Enqueue callback
	payload := map[string]interface{}{
		"message_id": "msg_123",
		"event":      "delivered",
		"email":      "test@example.com",
		"attempts":   1,
	}

	err := queue.EnqueueCallback("msg_123", "delivered", payload)
	if err != nil {
		t.Fatalf("Failed to enqueue callback: %v", err)
	}

	// Get pending callbacks
	pending, err := queue.GetPendingCallbacks(10)
	if err != nil {
		t.Fatalf("Failed to get pending callbacks: %v", err)
	}

	if len(pending) != 1 {
		t.Errorf("Expected 1 pending callback, got %d", len(pending))
	}

	if len(pending) > 0 {
		if pending[0].EventType != "delivered" {
			t.Errorf("Expected event_type 'delivered', got %s", pending[0].EventType)
		}

		// Mark as complete
		err = queue.MarkCallbackComplete(pending[0].ID)
		if err != nil {
			t.Fatalf("Failed to mark callback complete: %v", err)
		}

		// Verify no longer pending
		pending, _ = queue.GetPendingCallbacks(10)
		if len(pending) != 0 {
			t.Errorf("Expected 0 pending callbacks after completion, got %d", len(pending))
		}
	}
}

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		email    string
		expected string
	}{
		{"user@example.com", "example.com"},
		{"User@Example.COM", "example.com"},
		{"test@subdomain.example.com", "subdomain.example.com"},
		{"invalid-email", ""},
		{"@example.com", ""},
	}

	for _, tt := range tests {
		result := ExtractDomain(tt.email)
		if result != tt.expected {
			t.Errorf("ExtractDomain(%s) = %s, want %s", tt.email, result, tt.expected)
		}
	}
}
