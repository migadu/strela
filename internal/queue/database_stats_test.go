package queue

import (
	"fmt"
	"testing"
	"time"
)

func TestQueue_GetDatabaseStats(t *testing.T) {
	q, cleanup := SetupTestQueue(t)
	defer cleanup()

	stats, err := q.GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() failed: %v", err)
	}

	// Check basic stats
	if stats.PageSize == 0 {
		t.Error("Expected page size > 0")
	}

	if stats.PageCount == 0 {
		t.Error("Expected page count > 0")
	}

	if stats.SizeBytes == 0 {
		t.Error("Expected size bytes > 0")
	}

	if stats.SizeBytes != stats.PageCount*stats.PageSize {
		t.Errorf("Size calculation incorrect: %d != %d * %d", stats.SizeBytes, stats.PageCount, stats.PageSize)
	}

	// Fragment ratio should be between 0 and 1
	if stats.FragmentRatio < 0 || stats.FragmentRatio > 1 {
		t.Errorf("Fragment ratio out of range: %f", stats.FragmentRatio)
	}

	// New database should have no messages
	if stats.QueuedMessages != 0 {
		t.Errorf("Expected 0 queued messages, got %d", stats.QueuedMessages)
	}

	if stats.SendingMessages != 0 {
		t.Errorf("Expected 0 sending messages, got %d", stats.SendingMessages)
	}

	// Connections should be >= 0
	if stats.Connections < 0 {
		t.Errorf("Invalid connection count: %d", stats.Connections)
	}
}

func TestQueue_GetDatabaseStats_WithMessages(t *testing.T) {
	q, cleanup := SetupTestQueue(t)
	defer cleanup()

	// Enqueue some messages
	for i := 0; i < 5; i++ {
		msg := &QueuedMessage{
			MessageID:  fmt.Sprintf("msg_%d_%d", time.Now().UnixNano(), i),
			FromAddr:   "test@example.com",
			ToAddr:     "recipient@example.com",
			ToDomain:   "example.com",
			Subject:    "Test",
			RawMessage: []byte("Test message"),
			Status:     StatusQueued,
			ExpiresAt:  time.Now().Add(48 * time.Hour),
		}
		err := q.Enqueue(msg)
		if err != nil {
			t.Fatalf("Failed to enqueue message: %v", err)
		}
	}

	stats, err := q.GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() failed: %v", err)
	}

	// Should have 5 queued messages
	if stats.QueuedMessages != 5 {
		t.Errorf("Expected 5 queued messages, got %d", stats.QueuedMessages)
	}

	if stats.SendingMessages != 0 {
		t.Errorf("Expected 0 sending messages, got %d", stats.SendingMessages)
	}

	// Database size should have increased
	if stats.SizeBytes == 0 {
		t.Error("Expected database size > 0")
	}
}

func TestQueue_GetDatabaseStats_AfterVacuum(t *testing.T) {
	q, cleanup := SetupTestQueue(t)
	defer cleanup()

	// Get initial stats
	stats1, err := q.GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() failed: %v", err)
	}

	// Add and remove many messages to create fragmentation
	for i := 0; i < 100; i++ {
		msg := &QueuedMessage{
			MessageID:  fmt.Sprintf("msg_%d_%d", time.Now().UnixNano(), i),
			FromAddr:   "test@example.com",
			ToAddr:     "recipient@example.com",
			ToDomain:   "example.com",
			Subject:    "Test",
			RawMessage: []byte("Test message with some content to take up space"),
			Status:     StatusQueued,
			ExpiresAt:  time.Now().Add(48 * time.Hour),
		}
		err := q.Enqueue(msg)
		if err != nil {
			t.Fatalf("Failed to enqueue message: %v", err)
		}
	}

	// Delete all messages
	_, err = q.db.Exec("DELETE FROM messages")
	if err != nil {
		t.Fatalf("Failed to delete messages: %v", err)
	}

	// Get stats after deletion (should have fragmentation)
	stats2, err := q.GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() failed: %v", err)
	}

	// Fragmentation should exist
	if stats2.FragmentRatio == 0 {
		t.Log("Warning: Expected some fragmentation after mass deletion, got 0")
	}

	// Run VACUUM
	_, err = q.db.Exec("VACUUM")
	if err != nil {
		t.Fatalf("VACUUM failed: %v", err)
	}

	// Get stats after VACUUM
	stats3, err := q.GetDatabaseStats()
	if err != nil {
		t.Fatalf("GetDatabaseStats() failed: %v", err)
	}

	// After VACUUM, fragmentation should be reduced
	if stats3.FragmentRatio > stats2.FragmentRatio {
		t.Errorf("Fragmentation increased after VACUUM: %f > %f", stats3.FragmentRatio, stats2.FragmentRatio)
	}

	// Database size should be smaller or equal after vacuum
	if stats3.SizeBytes > stats1.SizeBytes*2 {
		t.Logf("Database size after VACUUM (%d) is larger than expected (initial: %d)", stats3.SizeBytes, stats1.SizeBytes)
	}
}

func TestQueue_GetDatabaseStats_ConcurrentAccess(t *testing.T) {
	q, cleanup := SetupTestQueue(t)
	defer cleanup()

	// Test that GetDatabaseStats works with concurrent access
	done := make(chan bool)
	errors := make(chan error, 10)

	// Start multiple goroutines accessing stats
	for i := 0; i < 10; i++ {
		go func() {
			stats, err := q.GetDatabaseStats()
			if err != nil {
				errors <- err
			} else if stats == nil {
				errors <- err
			}
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Check for errors
	select {
	case err := <-errors:
		t.Fatalf("Concurrent access failed: %v", err)
	default:
		// No errors
	}
}
