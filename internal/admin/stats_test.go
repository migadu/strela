package admin

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// setupTestDB creates a test database with schema
func setupTestDB(t *testing.T) (*sql.DB, string, func()) {
	dbPath := "./test_admin.db"

	// Remove if exists
	os.Remove(dbPath)

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	// Create schema
	schema := `
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT UNIQUE NOT NULL,
		status TEXT NOT NULL,
		from_addr TEXT NOT NULL,
		to_addr TEXT NOT NULL,
		to_domain TEXT NOT NULL,
		created_at TEXT NOT NULL,
		attempts INTEGER DEFAULT 0
	);

	CREATE TABLE delivery_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		attempted_at TEXT NOT NULL,
		mx_host TEXT,
		smtp_code INTEGER,
		smtp_response TEXT,
		error TEXT,
		error_category TEXT,
		success INTEGER NOT NULL
	);

	CREATE TABLE callback_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		event_type TEXT NOT NULL,
		created_at TEXT NOT NULL,
		completed_at TEXT
	);

	CREATE TABLE ip_reputation (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_ip TEXT UNIQUE NOT NULL,
		status TEXT NOT NULL,
		failure_count INTEGER DEFAULT 0,
		degraded_at TEXT,
		last_attempt_at TEXT,
		smtp_code INTEGER,
		smtp_response TEXT
	);
	`

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("Failed to create schema: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.Remove(dbPath)
	}

	return db, dbPath, cleanup
}

func TestNewAdminDB(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	db.Close()
	defer cleanup()

	adminDB, err := NewAdminDB(dbPath)
	if err != nil {
		t.Fatalf("NewAdminDB failed: %v", err)
	}
	defer adminDB.Close()

	if adminDB.db == nil {
		t.Error("Database connection is nil")
	}
}

func TestGetQueueStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	// Insert test data
	now := time.Now().Format(time.RFC3339)
	testData := []struct {
		messageID string
		status    string
		createdAt string
	}{
		{"msg1", "queued", now},
		{"msg2", "queued", now},
		{"msg3", "sending", now},
		{"msg4", "delivered", now},
		{"msg5", "hard_bounce", now},
		{"msg6", "expired", now},
		{"msg7", "temp_expired", now},
	}

	for _, td := range testData {
		_, err := db.Exec(`
			INSERT INTO messages (message_id, status, from_addr, to_addr, to_domain, created_at)
			VALUES (?, ?, 'sender@example.com', 'recipient@example.com', 'example.com', ?)
		`, td.messageID, td.status, td.createdAt)
		if err != nil {
			t.Fatalf("Failed to insert test data: %v", err)
		}
	}

	db.Close()

	// Test GetQueueStats
	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetQueueStats()
	if err != nil {
		t.Fatalf("GetQueueStats failed: %v", err)
	}

	if stats.Total != 7 {
		t.Errorf("Expected total=7, got %d", stats.Total)
	}
	if stats.Queued != 2 {
		t.Errorf("Expected queued=2, got %d", stats.Queued)
	}
	if stats.Sending != 1 {
		t.Errorf("Expected sending=1, got %d", stats.Sending)
	}
	if stats.Delivered != 1 {
		t.Errorf("Expected delivered=1, got %d", stats.Delivered)
	}
	if stats.HardBounce != 1 {
		t.Errorf("Expected hard_bounce=1, got %d", stats.HardBounce)
	}
	if stats.Expired != 1 {
		t.Errorf("Expected expired=1, got %d", stats.Expired)
	}
	if stats.TempExpired != 1 {
		t.Errorf("Expected temp_expired=1, got %d", stats.TempExpired)
	}

	if stats.OldestQueued == nil {
		t.Error("OldestQueued should not be nil")
	}
}

func TestGetDomainStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().Format(time.RFC3339)

	// Insert messages for different domains
	testData := []struct {
		domain string
		status string
		count  int
	}{
		{"example.com", "queued", 5},
		{"example.com", "sending", 2},
		{"test.com", "queued", 3},
		{"test.com", "hard_bounce", 1},
		{"another.com", "expired", 2},
	}

	msgID := 1
	for _, td := range testData {
		for i := 0; i < td.count; i++ {
			_, err := db.Exec(`
				INSERT INTO messages (message_id, status, from_addr, to_addr, to_domain, created_at)
				VALUES (?, ?, 'sender@example.com', 'recipient@example.com', ?, ?)
			`, fmt.Sprintf("msg%d", msgID), td.status, td.domain, now)
			if err != nil {
				t.Fatalf("Failed to insert test data: %v", err)
			}
			msgID++
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetDomainStats(10)
	if err != nil {
		t.Fatalf("GetDomainStats failed: %v", err)
	}

	if len(stats) != 3 {
		t.Errorf("Expected 3 domains, got %d", len(stats))
	}

	// Verify example.com stats (should be first with 7 total)
	if len(stats) > 0 {
		exampleStats := stats[0]
		if exampleStats.Domain != "example.com" {
			t.Errorf("Expected first domain to be example.com, got %s", exampleStats.Domain)
		}
		if exampleStats.Count != 7 {
			t.Errorf("Expected example.com count=7, got %d", exampleStats.Count)
		}
		if exampleStats.Queued != 5 {
			t.Errorf("Expected example.com queued=5, got %d", exampleStats.Queued)
		}
		if exampleStats.Sending != 2 {
			t.Errorf("Expected example.com sending=2, got %d", exampleStats.Sending)
		}
	}
}

func TestGetSenderStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().Format(time.RFC3339)

	// Insert messages from different senders
	testData := []struct {
		sender string
		status string
		count  int
	}{
		{"alice@example.com", "queued", 10},
		{"alice@example.com", "delivered", 5},
		{"bob@example.com", "queued", 3},
		{"bob@example.com", "hard_bounce", 2},
	}

	msgID := 1
	for _, td := range testData {
		for i := 0; i < td.count; i++ {
			_, err := db.Exec(`
				INSERT INTO messages (message_id, status, from_addr, to_addr, to_domain, created_at)
				VALUES (?, ?, ?, 'recipient@example.com', 'example.com', ?)
			`, fmt.Sprintf("msg%d", msgID), td.status, td.sender, now)
			if err != nil {
				t.Fatalf("Failed to insert test data: %v", err)
			}
			msgID++
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetSenderStats(10)
	if err != nil {
		t.Fatalf("GetSenderStats failed: %v", err)
	}

	if len(stats) != 2 {
		t.Errorf("Expected 2 senders, got %d", len(stats))
	}

	// Verify alice stats (should be first with 10 messages)
	if len(stats) > 0 {
		aliceStats := stats[0]
		if aliceStats.FromAddr != "alice@example.com" {
			t.Errorf("Expected first sender to be alice@example.com, got %s", aliceStats.FromAddr)
		}
		if aliceStats.Count != 10 {
			t.Errorf("Expected alice count=10, got %d", aliceStats.Count)
		}
		if aliceStats.Queued != 10 {
			t.Errorf("Expected alice queued=10, got %d", aliceStats.Queued)
		}
	}
}

func TestGetThroughputStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()

	// Insert delivery attempts at different times
	testData := []struct {
		timeOffset time.Duration
		success    int
	}{
		{-30 * time.Minute, 1}, // Last 1 hour
		{-30 * time.Minute, 1},
		{-3 * time.Hour, 1},      // Last 6 hours
		{-12 * time.Hour, 1},     // Last 24 hours
		{-48 * time.Hour, 1},     // Last 7 days
		{-30 * time.Minute, 0},   // Failed (last 1 hour)
		{-5 * time.Hour, 0},      // Failed (last 6 hours)
		{-200 * time.Hour, 1},    // Outside 7 days
	}

	for i, td := range testData {
		attemptTime := now.Add(td.timeOffset).Format(time.RFC3339)
		_, err := db.Exec(`
			INSERT INTO delivery_attempts (message_id, attempted_at, success, error_category)
			VALUES (?, ?, ?, 'network')
		`, fmt.Sprintf("msg%d", i), attemptTime, td.success)
		if err != nil {
			t.Fatalf("Failed to insert delivery attempt: %v", err)
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetThroughputStats()
	if err != nil {
		t.Fatalf("GetThroughputStats failed: %v", err)
	}

	if stats.Last1Hour != 3 {
		t.Errorf("Expected last_1h=3, got %d", stats.Last1Hour)
	}
	if stats.Last6Hours != 5 {
		t.Errorf("Expected last_6h=5, got %d", stats.Last6Hours)
	}
	if stats.Last24Hours != 6 {
		t.Errorf("Expected last_24h=6, got %d", stats.Last24Hours)
	}
	if stats.Last7Days != 7 {
		t.Errorf("Expected last_7d=7, got %d", stats.Last7Days)
	}
	if stats.TotalAttempts != 8 {
		t.Errorf("Expected total_attempts=8, got %d", stats.TotalAttempts)
	}

	// Success rate should be 6/8 = 75%
	expectedRate := 75.0
	if stats.SuccessRate < expectedRate-0.1 || stats.SuccessRate > expectedRate+0.1 {
		t.Errorf("Expected success rate ~%.1f%%, got %.1f%%", expectedRate, stats.SuccessRate)
	}
}

func TestGetRecentFailures(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()

	// Insert some failures
	testData := []struct {
		messageID    string
		mxHost       string
		smtpCode     int
		smtpResponse string
		error        string
		errorCat     string
		offset       time.Duration
	}{
		{"msg1", "mx1.example.com", 550, "User unknown", "Permanent error", "permanent", -1 * time.Minute},
		{"msg2", "mx2.example.com", 0, "", "Connection timeout", "network", -2 * time.Minute},
		{"msg3", "mx3.example.com", 421, "Try again later", "Greylisted", "temporary", -3 * time.Minute},
	}

	for _, td := range testData {
		attemptTime := now.Add(td.offset).Format(time.RFC3339)
		_, err := db.Exec(`
			INSERT INTO delivery_attempts
			(message_id, attempted_at, mx_host, smtp_code, smtp_response, error, error_category, success)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0)
		`, td.messageID, attemptTime, td.mxHost, td.smtpCode, td.smtpResponse, td.error, td.errorCat)
		if err != nil {
			t.Fatalf("Failed to insert failure: %v", err)
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	failures, err := adminDB.GetRecentFailures(10)
	if err != nil {
		t.Fatalf("GetRecentFailures failed: %v", err)
	}

	if len(failures) != 3 {
		t.Errorf("Expected 3 failures, got %d", len(failures))
	}

	// Should be ordered by most recent first
	if len(failures) > 0 {
		if failures[0].MessageID != "msg1" {
			t.Errorf("Expected first failure to be msg1, got %s", failures[0].MessageID)
		}
		if failures[0].MXHost != "mx1.example.com" {
			t.Errorf("Expected MX host mx1.example.com, got %s", failures[0].MXHost)
		}
		if failures[0].SMTPCode != 550 {
			t.Errorf("Expected SMTP code 550, got %d", failures[0].SMTPCode)
		}
		if failures[0].ErrorCategory != "permanent" {
			t.Errorf("Expected error category permanent, got %s", failures[0].ErrorCategory)
		}
	}
}

func TestGetCallbackStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().Format(time.RFC3339)

	// Insert callback queue entries
	testData := []struct {
		messageID   string
		eventType   string
		completedAt sql.NullString
	}{
		{"msg1", "delivered", sql.NullString{Valid: false}}, // Pending
		{"msg2", "delivered", sql.NullString{Valid: false}}, // Pending
		{"msg3", "hard_bounce", sql.NullString{String: now, Valid: true}}, // Completed
		{"msg4", "delivered", sql.NullString{String: now, Valid: true}},    // Completed
	}

	for _, td := range testData {
		var completedAt interface{}
		if td.completedAt.Valid {
			completedAt = td.completedAt.String
		}

		_, err := db.Exec(`
			INSERT INTO callback_queue (message_id, event_type, created_at, completed_at)
			VALUES (?, ?, ?, ?)
		`, td.messageID, td.eventType, now, completedAt)
		if err != nil {
			t.Fatalf("Failed to insert callback: %v", err)
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetCallbackStats()
	if err != nil {
		t.Fatalf("GetCallbackStats failed: %v", err)
	}

	if stats.Total != 4 {
		t.Errorf("Expected total=4, got %d", stats.Total)
	}
	if stats.Pending != 2 {
		t.Errorf("Expected pending=2, got %d", stats.Pending)
	}
	if stats.Completed != 2 {
		t.Errorf("Expected completed=2, got %d", stats.Completed)
	}
}

func TestGetIPReputationStats(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().Format(time.RFC3339)

	// Insert IP reputation entries
	testData := []struct {
		sourceIP     string
		status       string
		failureCount int
		degradedAt   sql.NullString
		smtpCode     int
	}{
		{"192.168.1.100", "degraded", 5, sql.NullString{String: now, Valid: true}, 550},
		{"192.168.1.101", "healthy", 0, sql.NullString{Valid: false}, 0},
		{"192.168.1.102", "degraded", 3, sql.NullString{String: now, Valid: true}, 554},
	}

	for _, td := range testData {
		var degradedAt interface{}
		if td.degradedAt.Valid {
			degradedAt = td.degradedAt.String
		}

		_, err := db.Exec(`
			INSERT INTO ip_reputation
			(source_ip, status, failure_count, degraded_at, last_attempt_at, smtp_code, smtp_response)
			VALUES (?, ?, ?, ?, ?, ?, 'Sender rejected')
		`, td.sourceIP, td.status, td.failureCount, degradedAt, now, td.smtpCode)
		if err != nil {
			t.Fatalf("Failed to insert IP reputation: %v", err)
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetIPReputationStats()
	if err != nil {
		t.Fatalf("GetIPReputationStats failed: %v", err)
	}

	if len(stats) != 3 {
		t.Errorf("Expected 3 IP entries, got %d", len(stats))
	}

	// Degraded IPs should come first
	degradedCount := 0
	for _, s := range stats {
		if s.Status == "degraded" {
			degradedCount++
			if s.DegradedAt == nil {
				t.Errorf("Degraded IP %s should have degraded_at time", s.SourceIP)
			}
		}
	}

	if degradedCount != 2 {
		t.Errorf("Expected 2 degraded IPs, got %d", degradedCount)
	}
}

func TestGetQueueStats_EmptyDatabase(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	db.Close()
	defer cleanup()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	stats, err := adminDB.GetQueueStats()
	if err != nil {
		t.Fatalf("GetQueueStats failed on empty DB: %v", err)
	}

	if stats.Total != 0 {
		t.Errorf("Expected total=0 on empty DB, got %d", stats.Total)
	}

	if stats.OldestQueued != nil {
		t.Error("OldestQueued should be nil on empty DB")
	}
}

func TestGetDomainStats_WithLimit(t *testing.T) {
	db, dbPath, cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now().Format(time.RFC3339)

	// Insert messages for 5 domains
	for i := 1; i <= 5; i++ {
		domain := fmt.Sprintf("domain%d.com", i)
		for j := 0; j < i; j++ {
			_, err := db.Exec(`
				INSERT INTO messages (message_id, status, from_addr, to_addr, to_domain, created_at)
				VALUES (?, 'queued', 'sender@example.com', 'recipient@example.com', ?, ?)
			`, fmt.Sprintf("msg%d_%d", i, j), domain, now)
			if err != nil {
				t.Fatalf("Failed to insert test data: %v", err)
			}
		}
	}

	db.Close()

	adminDB, _ := NewAdminDB(dbPath)
	defer adminDB.Close()

	// Request only top 3
	stats, err := adminDB.GetDomainStats(3)
	if err != nil {
		t.Fatalf("GetDomainStats failed: %v", err)
	}

	if len(stats) != 3 {
		t.Errorf("Expected 3 domains (limit), got %d", len(stats))
	}

	// Should be ordered by count descending
	if len(stats) >= 2 && stats[0].Count < stats[1].Count {
		t.Error("Domain stats should be ordered by count descending")
	}
}
