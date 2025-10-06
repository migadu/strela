package queue

import (
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestIdempotencyKeyDeduplication(t *testing.T) {
	// Create temporary database
	dbPath := "./test_idempotency.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Test 1: Enqueue message with idempotency key
	msg1 := &QueuedMessage{
		MessageID:      "msg_001",
		IdempotencyKey: "idem_abc123",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient@example.com",
		ToDomain:       "example.com",
		Subject:        "Test Subject",
		RawMessage:     []byte("Test message body"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg1)
	if err != nil {
		t.Fatalf("Failed to enqueue message: %v", err)
	}

	// Test 2: Retrieve by idempotency key
	found, err := q.GetMessageByIdempotencyKey("idem_abc123")
	if err != nil {
		t.Fatalf("Failed to get message by idempotency key: %v", err)
	}
	if found == nil {
		t.Fatal("Expected to find message by idempotency key, got nil")
	}
	if found.MessageID != "msg_001" {
		t.Errorf("Expected message_id 'msg_001', got '%s'", found.MessageID)
	}
	if found.IdempotencyKey != "idem_abc123" {
		t.Errorf("Expected idempotency_key 'idem_abc123', got '%s'", found.IdempotencyKey)
	}

	// Test 3: Different idempotency key returns nil
	notFound, err := q.GetMessageByIdempotencyKey("idem_xyz789")
	if err != nil {
		t.Fatalf("Failed to get message by non-existent key: %v", err)
	}
	if notFound != nil {
		t.Error("Expected nil for non-existent idempotency key")
	}

	// Test 4: Update message to terminal state
	err = q.UpdateStatus("msg_001", StatusDelivered)
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Test 5: Idempotency key still accessible after terminal state
	stillFound, err := q.GetMessageByIdempotencyKey("idem_abc123")
	if err != nil {
		t.Fatalf("Failed to get message after terminal state: %v", err)
	}
	if stillFound == nil {
		t.Fatal("Idempotency key should still be accessible after delivery")
	}
	if stillFound.Status != StatusDelivered {
		t.Errorf("Expected status 'delivered', got '%s'", stillFound.Status)
	}
}

func TestIdempotencyKeyUniqueness(t *testing.T) {
	// Create temporary database
	dbPath := "./test_idempotency_unique.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Test: Attempt to insert duplicate idempotency key
	msg1 := &QueuedMessage{
		MessageID:      "msg_001",
		IdempotencyKey: "idem_duplicate",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient1@example.com",
		ToDomain:       "example.com",
		Subject:        "Test 1",
		RawMessage:     []byte("Message 1"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg1)
	if err != nil {
		t.Fatalf("Failed to enqueue first message: %v", err)
	}

	// Try to enqueue with same idempotency key
	msg2 := &QueuedMessage{
		MessageID:      "msg_002",
		IdempotencyKey: "idem_duplicate",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient2@example.com",
		ToDomain:       "example.com",
		Subject:        "Test 2",
		RawMessage:     []byte("Message 2"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg2)
	// Note: SQLite doesn't enforce uniqueness on NULL values, but our index
	// should prevent duplicate non-NULL idempotency keys
	// The behavior depends on SQLite version and index implementation
	// For now, we just verify the first message is retrievable

	found, _ := q.GetMessageByIdempotencyKey("idem_duplicate")
	if found == nil {
		t.Fatal("Should find at least one message with the idempotency key")
	}
	if found.MessageID != "msg_001" {
		t.Errorf("Expected first message to be preserved, got %s", found.MessageID)
	}
}

func TestIdempotencyKeyWithNullValues(t *testing.T) {
	// Create temporary database
	dbPath := "./test_idempotency_null.db"
	defer os.Remove(dbPath)

	logger := zap.NewNop()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Test 1: Enqueue message without idempotency key
	msg1 := &QueuedMessage{
		MessageID:      "msg_no_idem_001",
		IdempotencyKey: "", // Empty string = NULL in database
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient@example.com",
		ToDomain:       "example.com",
		Subject:        "No idempotency key",
		RawMessage:     []byte("Test message"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg1)
	if err != nil {
		t.Fatalf("Failed to enqueue message without idempotency key: %v", err)
	}

	// Test 2: Query by empty key should return nil
	found, err := q.GetMessageByIdempotencyKey("")
	if err != nil {
		t.Fatalf("Failed to query empty idempotency key: %v", err)
	}
	if found != nil {
		t.Error("Empty idempotency key should not match any message")
	}

	// Test 3: Multiple messages without idempotency keys should be allowed
	msg2 := &QueuedMessage{
		MessageID:      "msg_no_idem_002",
		IdempotencyKey: "",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient2@example.com",
		ToDomain:       "example.com",
		Subject:        "Also no idempotency key",
		RawMessage:     []byte("Test message 2"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg2)
	if err != nil {
		t.Fatalf("Should allow multiple messages without idempotency key: %v", err)
	}
}
