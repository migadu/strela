package queue

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

// SetupTestQueue creates a temporary queue for testing
func SetupTestQueue(t *testing.T) (*Queue, func()) {
	t.Helper()

	// Create temporary database
	dbPath := "test_queue_" + time.Now().Format("20060102150405") + ".db"

	logger := slog.Default()
	queue, err := NewQueue(dbPath, logger)
	if err != nil {
		t.Fatalf("Failed to create queue: %v", err)
	}

	cleanup := func() {
		queue.Close()
		os.Remove(dbPath)
		os.Remove(dbPath + "-shm")
		os.Remove(dbPath + "-wal")
	}

	return queue, cleanup
}
