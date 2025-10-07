// Package admin provides read-only database access for the fune-admin CLI tool.
// It queries the SQLite database in read-only mode to retrieve queue statistics,
// delivery throughput, failure analysis, and IP reputation status.
//
// Key Features:
//   - Read-only database access (safe for production use)
//   - Queue statistics by message status
//   - Per-domain and per-sender message breakdowns
//   - Delivery throughput analysis (last 1h, 6h, 24h, 7d)
//   - Recent failure inspection with SMTP error details
//   - IP reputation status tracking
//   - Callback queue statistics
//   - Database health metrics (size, connections, fragmentation)
//
// Architecture:
//
// The admin package opens a separate read-only SQLite connection to avoid
// interfering with the main server's write operations. All queries use
// aggregation and filtering to provide actionable insights for operators.
//
// Example Usage:
//
//	db, err := admin.NewAdminDB("/var/lib/fune/queue.db")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer db.Close()
//
//	stats, err := db.GetQueueStats()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	fmt.Printf("Total messages: %d\n", stats.Total)
//	fmt.Printf("Queued: %d, Delivered: %d\n", stats.Queued, stats.Delivered)
//
//	// Get top domains by volume
//	domains, err := db.GetDomainStats(10)
//	for _, d := range domains {
//		fmt.Printf("%s: %d messages (%d queued, %d failed)\n",
//			d.Domain, d.Count, d.Queued, d.Failed)
//	}
package admin

import (
	"database/sql"
	"fmt"
	"time"

	"fune/internal/queue"

	_ "github.com/mattn/go-sqlite3"
)

// QueueStats represents aggregate queue statistics across all message statuses.
type QueueStats struct {
	Total        int64
	Queued       int64
	Sending      int64
	Delivered    int64
	HardBounce   int64
	TempExpired  int64
	Expired      int64
	OldestQueued *time.Time
}

// DomainStats represents message statistics grouped by recipient domain.
// Useful for identifying high-volume domains and delivery issues.
type DomainStats struct {
	Domain  string
	Count   int64
	Queued  int64
	Sending int64
	Failed  int64 // Hard bounces + expired
}

// SenderStats represents message statistics grouped by sender email address.
// Useful for identifying high-volume senders and per-sender delivery issues.
type SenderStats struct {
	FromAddr string
	Count    int64
	Queued   int64
	Sending  int64
	Failed   int64
}

// ThroughputStats represents delivery throughput over various time windows.
// Includes success rate for assessing overall delivery health.
type ThroughputStats struct {
	Last1Hour     int64
	Last6Hours    int64
	Last24Hours   int64
	Last7Days     int64
	SuccessRate   float64 // Percentage of successful deliveries
	TotalAttempts int64
}

// CircuitBreakerInfo represents circuit breaker state from the database.
// Not currently used but reserved for future circuit breaker persistence.
type CircuitBreakerInfo struct {
	Domain              string
	State               string
	ConsecutiveFailures int
	LastFailureTime     *time.Time
	LastStateChange     *time.Time
}

// AdminDB provides read-only database query functions for the admin CLI.
// Opens the database in read-only mode to prevent accidental writes.
type AdminDB struct {
	db *sql.DB
}

// NewAdminDB creates a new admin database connection in read-only mode.
// The database is opened with "mode=ro" to ensure safe concurrent access
// with the running server. Returns an error if the database file doesn't exist.
func NewAdminDB(dbPath string) (*AdminDB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro") // Read-only mode
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return &AdminDB{db: db}, nil
}

// Close closes the database connection. Should be called when the AdminDB
// is no longer needed to release resources.
func (a *AdminDB) Close() error {
	return a.db.Close()
}

// GetQueueStats retrieves overall queue statistics including message counts
// by status and the timestamp of the oldest queued message. Useful for
// monitoring queue depth and identifying stale messages.
func (a *AdminDB) GetQueueStats() (*QueueStats, error) {
	stats := &QueueStats{}

	// Get counts by status
	query := `
		SELECT
			COUNT(*) as total,
			COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0) as queued,
			COALESCE(SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END), 0) as sending,
			COALESCE(SUM(CASE WHEN status = 'delivered' THEN 1 ELSE 0 END), 0) as delivered,
			COALESCE(SUM(CASE WHEN status = 'hard_bounce' THEN 1 ELSE 0 END), 0) as hard_bounce,
			COALESCE(SUM(CASE WHEN status = 'temp_expired' THEN 1 ELSE 0 END), 0) as temp_expired,
			COALESCE(SUM(CASE WHEN status = 'expired' THEN 1 ELSE 0 END), 0) as expired
		FROM messages
	`

	err := a.db.QueryRow(query).Scan(
		&stats.Total,
		&stats.Queued,
		&stats.Sending,
		&stats.Delivered,
		&stats.HardBounce,
		&stats.TempExpired,
		&stats.Expired,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get queue stats: %w", err)
	}

	// Get oldest queued message
	var oldestStr sql.NullString
	err = a.db.QueryRow(`
		SELECT MIN(created_at)
		FROM messages
		WHERE status IN ('queued', 'sending')
	`).Scan(&oldestStr)

	if err == nil && oldestStr.Valid {
		oldest, _ := time.Parse(time.RFC3339, oldestStr.String)
		stats.OldestQueued = &oldest
	}

	return stats, nil
}

// GetDomainStats retrieves per-domain statistics for active messages
// (queued, sending, or failed), ordered by total message count descending.
// Limit specifies the maximum number of domains to return.
func (a *AdminDB) GetDomainStats(limit int) ([]DomainStats, error) {
	query := `
		SELECT
			to_domain,
			COUNT(*) as total,
			SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END) as queued,
			SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END) as sending,
			SUM(CASE WHEN status IN ('hard_bounce', 'expired', 'temp_expired') THEN 1 ELSE 0 END) as failed
		FROM messages
		WHERE status IN ('queued', 'sending', 'hard_bounce', 'expired', 'temp_expired')
		GROUP BY to_domain
		ORDER BY total DESC
		LIMIT ?
	`

	rows, err := a.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain stats: %w", err)
	}
	defer rows.Close()

	var stats []DomainStats
	for rows.Next() {
		var s DomainStats
		err := rows.Scan(&s.Domain, &s.Count, &s.Queued, &s.Sending, &s.Failed)
		if err != nil {
			return nil, fmt.Errorf("failed to scan domain stats: %w", err)
		}
		stats = append(stats, s)
	}

	return stats, nil
}

// GetSenderStats retrieves per-sender statistics for active messages
// (queued, sending, or failed), ordered by total message count descending.
// Limit specifies the maximum number of senders to return.
func (a *AdminDB) GetSenderStats(limit int) ([]SenderStats, error) {
	query := `
		SELECT
			from_addr,
			COUNT(*) as total,
			SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END) as queued,
			SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END) as sending,
			SUM(CASE WHEN status IN ('hard_bounce', 'expired', 'temp_expired') THEN 1 ELSE 0 END) as failed
		FROM messages
		WHERE status IN ('queued', 'sending', 'hard_bounce', 'expired', 'temp_expired')
		GROUP BY from_addr
		ORDER BY total DESC
		LIMIT ?
	`

	rows, err := a.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get sender stats: %w", err)
	}
	defer rows.Close()

	var stats []SenderStats
	for rows.Next() {
		var s SenderStats
		err := rows.Scan(&s.FromAddr, &s.Count, &s.Queued, &s.Sending, &s.Failed)
		if err != nil {
			return nil, fmt.Errorf("failed to scan sender stats: %w", err)
		}
		stats = append(stats, s)
	}

	return stats, nil
}

// GetThroughputStats retrieves delivery throughput statistics across
// multiple time windows (1h, 6h, 24h, 7d) and calculates overall success rate.
// Based on delivery_attempts table, not final message status.
func (a *AdminDB) GetThroughputStats() (*ThroughputStats, error) {
	stats := &ThroughputStats{}

	now := time.Now()

	// Count deliveries in different time windows
	query := `
		SELECT
			COUNT(CASE WHEN attempted_at >= ? THEN 1 END) as last_1h,
			COUNT(CASE WHEN attempted_at >= ? THEN 1 END) as last_6h,
			COUNT(CASE WHEN attempted_at >= ? THEN 1 END) as last_24h,
			COUNT(CASE WHEN attempted_at >= ? THEN 1 END) as last_7d,
			COUNT(*) as total,
			SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END) as successful
		FROM delivery_attempts
	`

	var successful int64
	err := a.db.QueryRow(query,
		now.Add(-1*time.Hour).Format(time.RFC3339),
		now.Add(-6*time.Hour).Format(time.RFC3339),
		now.Add(-24*time.Hour).Format(time.RFC3339),
		now.Add(-7*24*time.Hour).Format(time.RFC3339),
	).Scan(
		&stats.Last1Hour,
		&stats.Last6Hours,
		&stats.Last24Hours,
		&stats.Last7Days,
		&stats.TotalAttempts,
		&successful,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to get throughput stats: %w", err)
	}

	// Calculate success rate
	if stats.TotalAttempts > 0 {
		stats.SuccessRate = float64(successful) / float64(stats.TotalAttempts) * 100
	}

	return stats, nil
}

// GetRecentFailures retrieves recent delivery failures with SMTP error details,
// ordered by attempt timestamp descending. Useful for debugging delivery issues
// and identifying problematic recipient domains. Limit specifies max results.
func (a *AdminDB) GetRecentFailures(limit int) ([]FailureInfo, error) {
	query := `
		SELECT
			message_id,
			attempted_at,
			mx_host,
			smtp_code,
			smtp_response,
			error,
			error_category
		FROM delivery_attempts
		WHERE success = 0
		ORDER BY attempted_at DESC
		LIMIT ?
	`

	rows, err := a.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get recent failures: %w", err)
	}
	defer rows.Close()

	var failures []FailureInfo
	for rows.Next() {
		var f FailureInfo
		var attemptedAtStr string
		var mxHost, smtpResponse, errorStr, errorCategory sql.NullString
		var smtpCode sql.NullInt64

		err := rows.Scan(
			&f.MessageID,
			&attemptedAtStr,
			&mxHost,
			&smtpCode,
			&smtpResponse,
			&errorStr,
			&errorCategory,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan failure: %w", err)
		}

		f.AttemptedAt, _ = time.Parse(time.RFC3339, attemptedAtStr)
		if mxHost.Valid {
			f.MXHost = mxHost.String
		}
		if smtpCode.Valid {
			f.SMTPCode = int(smtpCode.Int64)
		}
		if smtpResponse.Valid {
			f.SMTPResponse = smtpResponse.String
		}
		if errorStr.Valid {
			f.Error = errorStr.String
		}
		if errorCategory.Valid {
			f.ErrorCategory = errorCategory.String
		}

		failures = append(failures, f)
	}

	return failures, nil
}

// FailureInfo represents a delivery failure record with SMTP error details.
// Used for failure analysis and debugging delivery issues.
type FailureInfo struct {
	MessageID     string
	AttemptedAt   time.Time
	MXHost        string
	SMTPCode      int
	SMTPResponse  string
	Error         string
	ErrorCategory string
}

// GetCallbackStats retrieves callback queue statistics including total callbacks,
// pending (not yet completed), and completed counts.
func (a *AdminDB) GetCallbackStats() (*CallbackStats, error) {
	stats := &CallbackStats{}

	query := `
		SELECT
			COUNT(*) as total,
			SUM(CASE WHEN completed_at IS NULL THEN 1 ELSE 0 END) as pending,
			SUM(CASE WHEN completed_at IS NOT NULL THEN 1 ELSE 0 END) as completed
		FROM callback_queue
	`

	err := a.db.QueryRow(query).Scan(&stats.Total, &stats.Pending, &stats.Completed)
	if err != nil {
		return nil, fmt.Errorf("failed to get callback stats: %w", err)
	}

	return stats, nil
}

// CallbackStats represents callback queue statistics. Useful for monitoring
// webhook delivery backlog and identifying webhook endpoint issues.
type CallbackStats struct {
	Total     int64
	Pending   int64
	Completed int64
}

// IPReputationInfo represents IP reputation status for a source IP address.
// Tracks degraded IPs and their failure reasons for reputation management.
type IPReputationInfo struct {
	SourceIP      string
	Status        string // "degraded" or "healthy"
	FailureCount  int
	DegradedAt    *time.Time
	LastAttemptAt *time.Time
	SMTPCode      int
	SMTPResponse  string
}

// GetDatabaseStats retrieves comprehensive database statistics including file
// size, WAL size, fragmentation ratio, and connection pool stats. This is a
// convenience function that opens a temporary read-only connection.
func GetDatabaseStats(dbPath string) (*queue.DatabaseStats, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	stats := &queue.DatabaseStats{}

	// Get page count and size
	err = db.QueryRow("PRAGMA page_count").Scan(&stats.PageCount)
	if err != nil {
		return nil, fmt.Errorf("failed to get page count: %w", err)
	}

	err = db.QueryRow("PRAGMA page_size").Scan(&stats.PageSize)
	if err != nil {
		return nil, fmt.Errorf("failed to get page size: %w", err)
	}

	stats.SizeBytes = stats.PageCount * stats.PageSize

	// Get freelist count (for fragmentation)
	var freelistCount int64
	err = db.QueryRow("PRAGMA freelist_count").Scan(&freelistCount)
	if err == nil && stats.PageCount > 0 {
		stats.FragmentRatio = float64(freelistCount) / float64(stats.PageCount)
	}

	// Get WAL file size (if exists)
	var walPages int64
	err = db.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(nil, &walPages, nil)
	if err == nil {
		stats.WALSizeBytes = walPages * stats.PageSize
	}

	// Get queue depth by status
	err = db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END), 0)
		FROM messages
	`).Scan(&stats.QueuedMessages, &stats.SendingMessages)
	if err != nil {
		return nil, fmt.Errorf("failed to get message counts: %w", err)
	}

	// Get connection pool stats
	dbStats := db.Stats()
	stats.Connections = dbStats.OpenConnections

	// Note: Cache hit ratio is not easily available in read-only mode
	// Setting to 0 as indicator that it's not available
	stats.CacheHitRatio = 0

	return stats, nil
}

// GetIPReputationStats retrieves IP reputation information for all tracked
// source IPs, ordered by status (degraded first) and degradation timestamp.
// Useful for monitoring IP reputation and identifying problematic IPs.
func (a *AdminDB) GetIPReputationStats() ([]IPReputationInfo, error) {
	query := `
		SELECT
			source_ip,
			status,
			failure_count,
			degraded_at,
			last_attempt_at,
			smtp_code,
			smtp_response
		FROM ip_reputation
		ORDER BY
			CASE WHEN status = 'degraded' THEN 0 ELSE 1 END,
			degraded_at DESC
	`

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get IP reputation stats: %w", err)
	}
	defer rows.Close()

	var stats []IPReputationInfo
	for rows.Next() {
		var info IPReputationInfo
		var degradedAtStr, lastAttemptStr sql.NullString
		var smtpCode sql.NullInt64
		var smtpResponse sql.NullString

		err := rows.Scan(
			&info.SourceIP,
			&info.Status,
			&info.FailureCount,
			&degradedAtStr,
			&lastAttemptStr,
			&smtpCode,
			&smtpResponse,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan IP reputation: %w", err)
		}

		if degradedAtStr.Valid {
			t, _ := time.Parse(time.RFC3339, degradedAtStr.String)
			info.DegradedAt = &t
		}
		if lastAttemptStr.Valid {
			t, _ := time.Parse(time.RFC3339, lastAttemptStr.String)
			info.LastAttemptAt = &t
		}
		if smtpCode.Valid {
			info.SMTPCode = int(smtpCode.Int64)
		}
		if smtpResponse.Valid {
			info.SMTPResponse = smtpResponse.String
		}

		stats = append(stats, info)
	}

	return stats, nil
}
