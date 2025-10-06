package admin

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// QueueStats represents queue statistics
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

// DomainStats represents per-domain statistics
type DomainStats struct {
	Domain  string
	Count   int64
	Queued  int64
	Sending int64
	Failed  int64 // Hard bounces + expired
}

// SenderStats represents per-sender statistics
type SenderStats struct {
	FromAddr string
	Count    int64
	Queued   int64
	Sending  int64
	Failed   int64
}

// ThroughputStats represents delivery throughput statistics
type ThroughputStats struct {
	Last1Hour     int64
	Last6Hours    int64
	Last24Hours   int64
	Last7Days     int64
	SuccessRate   float64 // Percentage of successful deliveries
	TotalAttempts int64
}

// CircuitBreakerInfo represents circuit breaker state from database
type CircuitBreakerInfo struct {
	Domain              string
	State               string
	ConsecutiveFailures int
	LastFailureTime     *time.Time
	LastStateChange     *time.Time
}

// AdminDB provides database query functions for admin CLI
type AdminDB struct {
	db *sql.DB
}

// NewAdminDB creates a new admin database connection
func NewAdminDB(dbPath string) (*AdminDB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro") // Read-only mode
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return &AdminDB{db: db}, nil
}

// Close closes the database connection
func (a *AdminDB) Close() error {
	return a.db.Close()
}

// GetQueueStats retrieves overall queue statistics
func (a *AdminDB) GetQueueStats() (*QueueStats, error) {
	stats := &QueueStats{}

	// Get counts by status
	query := `
		SELECT
			COUNT(*) as total,
			SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END) as queued,
			SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END) as sending,
			SUM(CASE WHEN status = 'delivered' THEN 1 ELSE 0 END) as delivered,
			SUM(CASE WHEN status = 'hard_bounce' THEN 1 ELSE 0 END) as hard_bounce,
			SUM(CASE WHEN status = 'temp_expired' THEN 1 ELSE 0 END) as temp_expired,
			SUM(CASE WHEN status = 'expired' THEN 1 ELSE 0 END) as expired
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

// GetDomainStats retrieves per-domain statistics
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

// GetSenderStats retrieves per-sender statistics
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

// GetThroughputStats retrieves delivery throughput statistics
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

// GetRecentFailures retrieves recent delivery failures
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

// FailureInfo represents a delivery failure record
type FailureInfo struct {
	MessageID     string
	AttemptedAt   time.Time
	MXHost        string
	SMTPCode      int
	SMTPResponse  string
	Error         string
	ErrorCategory string
}

// GetCallbackStats retrieves callback queue statistics
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

// CallbackStats represents callback queue statistics
type CallbackStats struct {
	Total     int64
	Pending   int64
	Completed int64
}

// IPReputationInfo represents IP reputation status
type IPReputationInfo struct {
	SourceIP      string
	Status        string // "degraded" or "healthy"
	FailureCount  int
	DegradedAt    *time.Time
	LastAttemptAt *time.Time
	SMTPCode      int
	SMTPResponse  string
}

// GetIPReputationStats retrieves IP reputation information
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
