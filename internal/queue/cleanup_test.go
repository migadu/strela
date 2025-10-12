package queue

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestCleanupTerminalMessages_WithIdempotencyKey(t *testing.T) {
	// Create temporary database
	dbPath := "./test_cleanup_with_idem.db"
	defer os.Remove(dbPath)

	logger := slog.Default()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Enqueue message with idempotency key
	msg := &QueuedMessage{
		MessageID:      "msg_idem_001",
		IdempotencyKey: "idem_test_001",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient@example.com",
		ToDomain:       "example.com",
		Subject:        "Test",
		RawMessage:     []byte("Test message"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg)
	if err != nil {
		t.Fatalf("Failed to enqueue: %v", err)
	}

	// Update to terminal state
	err = q.UpdateStatus("msg_idem_001", StatusDelivered)
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Test 1: Cleanup with TTL > message age (should NOT delete)
	deleted, err := q.CleanupTerminalMessages(24) // 24 hour TTL
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if deleted != 0 {
		t.Errorf("Should not delete message within TTL, deleted %d", deleted)
	}

	// Verify message still exists
	found, _ := q.GetMessageByIdempotencyKey("idem_test_001")
	if found == nil {
		t.Error("Message should still exist within TTL period")
	}

	// Test 2: Cleanup with TTL = 0 (should delete immediately)
	deleted, err = q.CleanupTerminalMessages(0)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("Should delete 1 message with TTL=0, deleted %d", deleted)
	}

	// Verify message is deleted
	notFound, _ := q.GetMessageByIdempotencyKey("idem_test_001")
	if notFound != nil {
		t.Error("Message should be deleted after TTL expires")
	}
}

func TestCleanupTerminalMessages_WithoutIdempotencyKey(t *testing.T) {
	// Create temporary database
	dbPath := "./test_cleanup_no_idem.db"
	defer os.Remove(dbPath)

	logger := slog.Default()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Enqueue message WITHOUT idempotency key
	msg := &QueuedMessage{
		MessageID:      "msg_no_idem_001",
		IdempotencyKey: "", // No idempotency key
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient@example.com",
		ToDomain:       "example.com",
		Subject:        "Test",
		RawMessage:     []byte("Test message"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg)
	if err != nil {
		t.Fatalf("Failed to enqueue: %v", err)
	}

	// Update to terminal state
	err = q.UpdateStatus("msg_no_idem_001", StatusDelivered)
	if err != nil {
		t.Fatalf("Failed to update status: %v", err)
	}

	// Test: Cleanup should delete immediately (no idempotency key = no TTL)
	deleted, err := q.CleanupTerminalMessages(24) // TTL doesn't matter for non-idempotent
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("Should delete message without idempotency key immediately, deleted %d", deleted)
	}

	// Verify message is deleted
	notFound, _ := q.GetMessage("msg_no_idem_001")
	if notFound != nil {
		t.Error("Message without idempotency key should be deleted immediately")
	}
}

func TestCleanupTerminalMessages_OnlyTerminalStates(t *testing.T) {
	// Create temporary database
	dbPath := "./test_cleanup_states.db"
	defer os.Remove(dbPath)

	logger := slog.Default()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Create messages in different states
	states := []struct {
		messageID       string
		status          MessageStatus
		shouldBeDeleted bool
	}{
		{"msg_queued", StatusQueued, false},
		{"msg_sending", StatusSending, false},
		{"msg_delivered", StatusDelivered, true},
		{"msg_hard_bounce", StatusHardBounce, true},
		{"msg_temp_expired", StatusTempExpired, true},
		{"msg_expired", StatusExpired, true},
	}

	for _, tc := range states {
		msg := &QueuedMessage{
			MessageID:      tc.messageID,
			IdempotencyKey: "", // No idempotency key for immediate cleanup
			FromAddr:       "sender@example.com",
			ToAddr:         "recipient@example.com",
			ToDomain:       "example.com",
			Subject:        "Test",
			RawMessage:     []byte("Test message"),
			ExpiresAt:      time.Now().Add(48 * time.Hour),
		}
		err = q.Enqueue(msg)
		if err != nil {
			t.Fatalf("Failed to enqueue %s: %v", tc.messageID, err)
		}

		err = q.UpdateStatus(tc.messageID, tc.status)
		if err != nil {
			t.Fatalf("Failed to update status for %s: %v", tc.messageID, err)
		}
	}

	// Run cleanup
	deleted, err := q.CleanupTerminalMessages(0)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	// Should delete only terminal states (delivered, hard_bounce, temp_expired, expired)
	expectedDeleted := int64(4)
	if deleted != expectedDeleted {
		t.Errorf("Expected to delete %d messages, deleted %d", expectedDeleted, deleted)
	}

	// Verify non-terminal messages still exist
	for _, tc := range states {
		found, _ := q.GetMessage(tc.messageID)
		if tc.shouldBeDeleted {
			if found != nil {
				t.Errorf("Message %s (%s) should be deleted", tc.messageID, tc.status)
			}
		} else {
			if found == nil {
				t.Errorf("Message %s (%s) should NOT be deleted", tc.messageID, tc.status)
			}
		}
	}
}

func TestCleanupTerminalMessages_MixedIdempotencyKeys(t *testing.T) {
	// Create temporary database
	dbPath := "./test_cleanup_mixed.db"
	defer os.Remove(dbPath)

	logger := slog.Default()
	q, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}
	defer q.Close()

	// Message 1: With idempotency key (should be kept during TTL)
	msg1 := &QueuedMessage{
		MessageID:      "msg_with_idem",
		IdempotencyKey: "idem_keep",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient1@example.com",
		ToDomain:       "example.com",
		Subject:        "Test 1",
		RawMessage:     []byte("Test message 1"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	// Message 2: Without idempotency key (should be deleted immediately)
	msg2 := &QueuedMessage{
		MessageID:      "msg_without_idem",
		IdempotencyKey: "",
		FromAddr:       "sender@example.com",
		ToAddr:         "recipient2@example.com",
		ToDomain:       "example.com",
		Subject:        "Test 2",
		RawMessage:     []byte("Test message 2"),
		ExpiresAt:      time.Now().Add(48 * time.Hour),
	}

	err = q.Enqueue(msg1)
	if err != nil {
		t.Fatalf("Failed to enqueue msg1: %v", err)
	}

	err = q.Enqueue(msg2)
	if err != nil {
		t.Fatalf("Failed to enqueue msg2: %v", err)
	}

	// Both to terminal state
	q.UpdateStatus("msg_with_idem", StatusDelivered)
	q.UpdateStatus("msg_without_idem", StatusDelivered)

	// Cleanup with 24h TTL
	deleted, err := q.CleanupTerminalMessages(24)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	// Should only delete message without idempotency key
	if deleted != 1 {
		t.Errorf("Should delete only 1 message (without idempotency key), deleted %d", deleted)
	}

	// Verify msg1 still exists (has idempotency key within TTL)
	found1, _ := q.GetMessageByIdempotencyKey("idem_keep")
	if found1 == nil {
		t.Error("Message with idempotency key should be kept within TTL")
	}

	// Verify msg2 is deleted (no idempotency key)
	found2, _ := q.GetMessage("msg_without_idem")
	if found2 != nil {
		t.Error("Message without idempotency key should be deleted immediately")
	}
}
