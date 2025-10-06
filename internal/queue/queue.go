package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// MetricsRecorder interface for recording queue metrics
type MetricsRecorder interface {
	RecordQueueDepth(status string, count int64)
}

// Queue manages the message queue in SQLite
type Queue struct {
	db               *sql.DB
	logger           *zap.Logger
	notifyCh         chan struct{}   // Channel to notify workers of new messages
	callbackNotifyCh chan struct{}   // Channel to notify callback processor of new callbacks
	writeMu          sync.Mutex      // Mutex to serialize write operations
	metrics          MetricsRecorder // Optional metrics recorder
}

// QueuedMessage represents a message in the queue
type QueuedMessage struct {
	ID        int64
	MessageID string

	// Idempotency (optional)
	IdempotencyKey string

	// Message data
	FromAddr   string
	ToAddr     string
	ToDomain   string
	Subject    string
	RawMessage []byte

	// DKIM signing (optional)
	DKIMPrivateKey string
	DKIMSelector   string
	DKIMDomain     string

	// Queue state
	Status      MessageStatus
	Attempts    int
	NextRetryAt time.Time

	// Timestamps
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time

	// Delivery results
	LastError        string
	LastSMTPCode     int
	LastSMTPResponse string
	FinalMXHost      string
	SourceIP         string
}

// MessageStatus represents the state of a message
type MessageStatus string

const (
	StatusQueued      MessageStatus = "queued"
	StatusSending     MessageStatus = "sending"
	StatusDelivered   MessageStatus = "delivered"
	StatusHardBounce  MessageStatus = "hard_bounce"
	StatusTempExpired MessageStatus = "temp_expired"
	StatusExpired     MessageStatus = "expired"
)

// DeliveryAttempt records a single delivery attempt
type DeliveryAttempt struct {
	ID            int64
	MessageID     string
	AttemptNumber int
	AttemptedAt   time.Time

	MXHost   string
	SourceIP string

	SMTPCode      int
	SMTPResponse  string
	Error         string
	Success       bool
	DurationMs    int64
	ErrorCategory string
}

// NewQueue creates a new queue instance
func NewQueue(dbPath string, logger *zap.Logger) (*Queue, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set connection pool settings for WAL mode
	// WAL mode supports one writer + multiple concurrent readers
	db.SetMaxOpenConns(10)   // Allow multiple concurrent read connections
	db.SetMaxIdleConns(5)    // Keep some idle connections for reuse
	db.SetConnMaxLifetime(0) // Connections don't expire

	q := &Queue{
		db:               db,
		logger:           logger,
		notifyCh:         make(chan struct{}, 100), // Buffered channel to prevent blocking
		callbackNotifyCh: make(chan struct{}, 100), // Buffered channel for callback notifications
	}

	if err := q.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return q, nil
}

// initSchema creates the database schema
func (q *Queue) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT UNIQUE NOT NULL,
		idempotency_key TEXT,

		from_addr TEXT NOT NULL,
		to_addr TEXT NOT NULL,
		to_domain TEXT NOT NULL,
		subject TEXT,
		raw_message BLOB NOT NULL,

		dkim_private_key TEXT,
		dkim_selector TEXT,
		dkim_domain TEXT,

		status TEXT NOT NULL CHECK(status IN ('queued', 'sending', 'delivered', 'hard_bounce', 'temp_expired', 'expired')),
		attempts INTEGER DEFAULT 0,
		next_retry_at TIMESTAMP NOT NULL,

		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		expires_at TIMESTAMP NOT NULL,

		last_error TEXT,
		last_smtp_code INTEGER,
		last_smtp_response TEXT,
		final_mx_host TEXT,
		source_ip TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_status_retry ON messages(status, next_retry_at) WHERE status IN ('queued', 'sending');
	CREATE INDEX IF NOT EXISTS idx_expires ON messages(expires_at) WHERE status IN ('queued', 'sending');
	CREATE INDEX IF NOT EXISTS idx_message_id ON messages(message_id);
	CREATE INDEX IF NOT EXISTS idx_idempotency_key ON messages(idempotency_key) WHERE idempotency_key IS NOT NULL;

	CREATE TABLE IF NOT EXISTS delivery_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		attempt_number INTEGER NOT NULL,
		attempted_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

		mx_host TEXT,
		source_ip TEXT,

		smtp_code INTEGER,
		smtp_response TEXT,
		error TEXT,
		success BOOLEAN,
		duration_ms INTEGER,
		error_category TEXT CHECK(error_category IS NULL OR error_category IN ('temporary', 'permanent', 'greylist', 'network')),

		FOREIGN KEY (message_id) REFERENCES messages(message_id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_attempts_message ON delivery_attempts(message_id);

	CREATE TABLE IF NOT EXISTS callback_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,

		event_type TEXT NOT NULL CHECK(event_type IN ('delivered', 'hard_bounce', 'temp_expired', 'expired')),
		payload TEXT NOT NULL,

		attempts INTEGER DEFAULT 0,
		next_retry_at TIMESTAMP NOT NULL,

		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		completed_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_callback_retry ON callback_queue(next_retry_at) WHERE completed_at IS NULL;

	CREATE TABLE IF NOT EXISTS mx_cache (
		domain TEXT PRIMARY KEY,
		mx_records TEXT NOT NULL,
		cached_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		ttl_seconds INTEGER DEFAULT 3600
	);

	CREATE INDEX IF NOT EXISTS idx_mx_ttl ON mx_cache(cached_at, ttl_seconds);
	`

	_, err := q.db.Exec(schema)
	return err
}

// Enqueue adds a new message to the queue
func (q *Queue) Enqueue(msg *QueuedMessage) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		INSERT INTO messages (
			message_id, idempotency_key, from_addr, to_addr, to_domain, subject, raw_message,
			dkim_private_key, dkim_selector, dkim_domain,
			status, attempts, next_retry_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	var idempotencyKey interface{}
	if msg.IdempotencyKey != "" {
		idempotencyKey = msg.IdempotencyKey
	} else {
		idempotencyKey = nil
	}

	_, err := q.db.Exec(query,
		msg.MessageID,
		idempotencyKey,
		msg.FromAddr,
		msg.ToAddr,
		msg.ToDomain,
		msg.Subject,
		msg.RawMessage,
		msg.DKIMPrivateKey,
		msg.DKIMSelector,
		msg.DKIMDomain,
		StatusQueued,
		0,
		time.Now().Format(time.RFC3339), // next_retry_at = now (immediate)
		msg.ExpiresAt.Format(time.RFC3339),
	)

	if err != nil {
		q.logger.Error("failed to enqueue message",
			zap.String("message_id", msg.MessageID),
			zap.Error(err))
		return fmt.Errorf("failed to enqueue: %w", err)
	}

	q.logger.Info("message enqueued",
		zap.String("message_id", msg.MessageID),
		zap.String("to", msg.ToAddr),
		zap.Time("expires_at", msg.ExpiresAt))

	// Notify workers of new message (non-blocking)
	select {
	case q.notifyCh <- struct{}{}:
	default:
		// Channel full, workers will pick it up on next poll
	}

	return nil
}

// GetNextMessages retrieves messages ready for processing
func (q *Queue) GetNextMessages(limit int) ([]*QueuedMessage, error) {
	query := `
		SELECT
			id, message_id, from_addr, to_addr, to_domain, subject, raw_message,
			dkim_private_key, dkim_selector, dkim_domain,
			status, attempts, next_retry_at, created_at, updated_at, expires_at,
			last_error, last_smtp_code, last_smtp_response, final_mx_host, source_ip
		FROM messages
		WHERE status = 'queued'
		  AND next_retry_at <= ?
		  AND expires_at > ?
		ORDER BY next_retry_at ASC
		LIMIT ?
	`

	now := time.Now().Format(time.RFC3339)
	rows, err := q.db.Query(query, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %w", err)
	}
	defer rows.Close()

	var messages []*QueuedMessage
	for rows.Next() {
		msg := &QueuedMessage{}
		var nextRetryAt, createdAt, updatedAt, expiresAt string
		var dkimPrivateKey, dkimSelector, dkimDomain sql.NullString
		var lastError, lastSMTPResponse, finalMXHost, sourceIP sql.NullString
		var lastSMTPCode sql.NullInt64

		err := rows.Scan(
			&msg.ID, &msg.MessageID, &msg.FromAddr, &msg.ToAddr, &msg.ToDomain,
			&msg.Subject, &msg.RawMessage,
			&dkimPrivateKey, &dkimSelector, &dkimDomain,
			&msg.Status, &msg.Attempts,
			&nextRetryAt, &createdAt, &updatedAt, &expiresAt,
			&lastError, &lastSMTPCode, &lastSMTPResponse, &finalMXHost, &sourceIP,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}

		// Parse timestamps (RFC3339 format)
		msg.NextRetryAt, _ = time.Parse(time.RFC3339, nextRetryAt)
		msg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		msg.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		msg.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)

		// Handle nullable fields
		if dkimPrivateKey.Valid {
			msg.DKIMPrivateKey = dkimPrivateKey.String
		}
		if dkimSelector.Valid {
			msg.DKIMSelector = dkimSelector.String
		}
		if dkimDomain.Valid {
			msg.DKIMDomain = dkimDomain.String
		}
		if lastError.Valid {
			msg.LastError = lastError.String
		}
		if lastSMTPCode.Valid {
			msg.LastSMTPCode = int(lastSMTPCode.Int64)
		}
		if lastSMTPResponse.Valid {
			msg.LastSMTPResponse = lastSMTPResponse.String
		}
		if finalMXHost.Valid {
			msg.FinalMXHost = finalMXHost.String
		}
		if sourceIP.Valid {
			msg.SourceIP = sourceIP.String
		}

		messages = append(messages, msg)
	}

	return messages, nil
}

// UpdateStatus updates the message status
func (q *Queue) UpdateStatus(messageID string, status MessageStatus) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		UPDATE messages
		SET status = ?, updated_at = ?
		WHERE message_id = ?
	`

	_, err := q.db.Exec(query, status, time.Now().Format(time.RFC3339), messageID)
	if err != nil {
		q.logger.Error("failed to update status",
			zap.String("message_id", messageID),
			zap.String("status", string(status)),
			zap.Error(err))
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

// ScheduleRetry updates message for retry
func (q *Queue) ScheduleRetry(messageID string, nextRetry time.Time, attempts int, lastError string, smtpCode int, smtpResponse string) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		UPDATE messages
		SET status = 'queued',
		    attempts = ?,
		    next_retry_at = ?,
		    last_error = ?,
		    last_smtp_code = ?,
		    last_smtp_response = ?,
		    updated_at = ?
		WHERE message_id = ?
	`

	_, err := q.db.Exec(query, attempts, nextRetry.Format(time.RFC3339), lastError, smtpCode, smtpResponse, time.Now().Format(time.RFC3339), messageID)
	if err != nil {
		q.logger.Error("failed to schedule retry",
			zap.String("message_id", messageID),
			zap.Time("next_retry", nextRetry),
			zap.Error(err))
		return fmt.Errorf("failed to schedule retry: %w", err)
	}

	q.logger.Info("retry scheduled",
		zap.String("message_id", messageID),
		zap.Int("attempt", attempts),
		zap.Time("next_retry", nextRetry))

	return nil
}

// RecordAttempt logs a delivery attempt
func (q *Queue) RecordAttempt(attempt *DeliveryAttempt) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		INSERT INTO delivery_attempts (
			message_id, attempt_number, attempted_at, mx_host, source_ip,
			smtp_code, smtp_response, error, success, duration_ms, error_category
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	// Convert empty ErrorCategory to nil for successful attempts
	var errorCategory interface{}
	if attempt.ErrorCategory == "" {
		errorCategory = nil
	} else {
		errorCategory = attempt.ErrorCategory
	}

	_, err := q.db.Exec(query,
		attempt.MessageID,
		attempt.AttemptNumber,
		attempt.AttemptedAt,
		attempt.MXHost,
		attempt.SourceIP,
		attempt.SMTPCode,
		attempt.SMTPResponse,
		attempt.Error,
		attempt.Success,
		attempt.DurationMs,
		errorCategory,
	)

	if err != nil {
		q.logger.Error("failed to record attempt",
			zap.String("message_id", attempt.MessageID),
			zap.Error(err))
		return fmt.Errorf("failed to record attempt: %w", err)
	}

	return nil
}

// UpdateDeliveryResult updates final delivery details
func (q *Queue) UpdateDeliveryResult(messageID string, mxHost string, sourceIP string, smtpCode int, smtpResponse string) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		UPDATE messages
		SET final_mx_host = ?,
		    source_ip = ?,
		    last_smtp_code = ?,
		    last_smtp_response = ?,
		    updated_at = ?
		WHERE message_id = ?
	`

	_, err := q.db.Exec(query, mxHost, sourceIP, smtpCode, smtpResponse, time.Now().Format(time.RFC3339), messageID)
	return err
}

// DeleteMessage removes a message from the queue
func (q *Queue) DeleteMessage(messageID string) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	_, err := q.db.Exec("DELETE FROM messages WHERE message_id = ?", messageID)
	if err != nil {
		q.logger.Error("failed to delete message",
			zap.String("message_id", messageID),
			zap.Error(err))
		return fmt.Errorf("failed to delete message: %w", err)
	}

	q.logger.Debug("message deleted from queue",
		zap.String("message_id", messageID))

	return nil
}

// FindExpiredMessages finds messages past their expiration time
func (q *Queue) FindExpiredMessages() ([]*QueuedMessage, error) {
	query := `
		SELECT
			id, message_id, from_addr, to_addr, to_domain, subject,
			status, attempts, created_at, expires_at
		FROM messages
		WHERE status IN ('queued', 'sending')
		  AND expires_at <= ?
	`

	rows, err := q.db.Query(query, time.Now().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("failed to find expired messages: %w", err)
	}
	defer rows.Close()

	var messages []*QueuedMessage
	for rows.Next() {
		msg := &QueuedMessage{}
		var createdAt, expiresAt string

		err := rows.Scan(
			&msg.ID, &msg.MessageID, &msg.FromAddr, &msg.ToAddr, &msg.ToDomain,
			&msg.Subject, &msg.Status, &msg.Attempts, &createdAt, &expiresAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan expired message: %w", err)
		}

		msg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		msg.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)

		messages = append(messages, msg)
	}

	return messages, nil
}

// EnqueueCallback adds a callback to the callback queue
func (q *Queue) EnqueueCallback(messageID string, eventType string, payload interface{}) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	query := `
		INSERT INTO callback_queue (
			message_id, event_type, payload, attempts, next_retry_at
		) VALUES (?, ?, ?, 0, ?)
	`

	_, err = q.db.Exec(query, messageID, eventType, string(jsonPayload), time.Now().Format(time.RFC3339))
	if err != nil {
		q.logger.Error("failed to enqueue callback",
			zap.String("message_id", messageID),
			zap.String("event_type", eventType),
			zap.Error(err))
		return fmt.Errorf("failed to enqueue callback: %w", err)
	}

	q.logger.Debug("callback enqueued",
		zap.String("message_id", messageID),
		zap.String("event_type", eventType))

	// Notify callback processor (non-blocking)
	select {
	case q.callbackNotifyCh <- struct{}{}:
	default:
		// Channel full, processor will pick it up on next poll
	}

	return nil
}

// GetPendingCallbacks retrieves callbacks ready to send
func (q *Queue) GetPendingCallbacks(limit int) ([]PendingCallback, error) {
	query := `
		SELECT id, message_id, event_type, payload, attempts, created_at
		FROM callback_queue
		WHERE completed_at IS NULL
		  AND next_retry_at <= ?
		ORDER BY next_retry_at ASC
		LIMIT ?
	`

	rows, err := q.db.Query(query, time.Now().Format(time.RFC3339), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending callbacks: %w", err)
	}
	defer rows.Close()

	var callbacks []PendingCallback
	for rows.Next() {
		var cb PendingCallback
		var createdAtStr string
		err := rows.Scan(&cb.ID, &cb.MessageID, &cb.EventType, &cb.Payload, &cb.Attempts, &createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("failed to scan callback: %w", err)
		}
		// Parse created_at timestamp
		cb.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
		callbacks = append(callbacks, cb)
	}

	return callbacks, nil
}

// PendingCallback represents a callback waiting to be sent
type PendingCallback struct {
	ID        int64
	MessageID string
	EventType string
	Payload   string
	Attempts  int
	CreatedAt time.Time
}

// MarkCallbackComplete marks a callback as successfully sent
func (q *Queue) MarkCallbackComplete(callbackID int64) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	_, err := q.db.Exec("UPDATE callback_queue SET completed_at = ? WHERE id = ?", time.Now().Format(time.RFC3339), callbackID)
	return err
}

// ScheduleCallbackRetry schedules a callback retry
func (q *Queue) ScheduleCallbackRetry(callbackID int64, nextRetry time.Time) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	_, err := q.db.Exec("UPDATE callback_queue SET attempts = attempts + 1, next_retry_at = ? WHERE id = ?", nextRetry.Format(time.RFC3339), callbackID)
	return err
}

// GetMessageByIdempotencyKey retrieves a message by idempotency key
func (q *Queue) GetMessageByIdempotencyKey(idempotencyKey string) (*QueuedMessage, error) {
	query := `
		SELECT
			id, message_id, from_addr, to_addr, to_domain, subject,
			status, attempts, created_at, updated_at
		FROM messages
		WHERE idempotency_key = ?
		LIMIT 1
	`

	msg := &QueuedMessage{}
	var createdAt, updatedAt string

	err := q.db.QueryRow(query, idempotencyKey).Scan(
		&msg.ID, &msg.MessageID, &msg.FromAddr, &msg.ToAddr, &msg.ToDomain,
		&msg.Subject, &msg.Status, &msg.Attempts, &createdAt, &updatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get message by idempotency key: %w", err)
	}

	// Parse timestamps
	msg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	msg.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	msg.IdempotencyKey = idempotencyKey

	return msg, nil
}

// GetMessage retrieves a single message by ID
func (q *Queue) GetMessage(messageID string) (*QueuedMessage, error) {
	query := `
		SELECT
			id, message_id, from_addr, to_addr, to_domain, subject, raw_message,
			dkim_private_key, dkim_selector, dkim_domain,
			status, attempts, next_retry_at, created_at, updated_at, expires_at,
			last_error, last_smtp_code, last_smtp_response, final_mx_host, source_ip
		FROM messages
		WHERE message_id = ?
	`

	msg := &QueuedMessage{}
	var nextRetryAt, createdAt, updatedAt, expiresAt string
	var dkimPrivateKey, dkimSelector, dkimDomain sql.NullString
	var lastError, lastSMTPResponse, finalMXHost, sourceIP sql.NullString
	var lastSMTPCode sql.NullInt64

	err := q.db.QueryRow(query, messageID).Scan(
		&msg.ID, &msg.MessageID, &msg.FromAddr, &msg.ToAddr, &msg.ToDomain,
		&msg.Subject, &msg.RawMessage,
		&dkimPrivateKey, &dkimSelector, &dkimDomain,
		&msg.Status, &msg.Attempts,
		&nextRetryAt, &createdAt, &updatedAt, &expiresAt,
		&lastError, &lastSMTPCode, &lastSMTPResponse, &finalMXHost, &sourceIP,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get message: %w", err)
	}

	// Parse timestamps (RFC3339 format)
	msg.NextRetryAt, _ = time.Parse(time.RFC3339, nextRetryAt)
	msg.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	msg.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	msg.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)

	// Handle nullable fields
	if dkimPrivateKey.Valid {
		msg.DKIMPrivateKey = dkimPrivateKey.String
	}
	if dkimSelector.Valid {
		msg.DKIMSelector = dkimSelector.String
	}
	if dkimDomain.Valid {
		msg.DKIMDomain = dkimDomain.String
	}
	if lastError.Valid {
		msg.LastError = lastError.String
	}
	if lastSMTPCode.Valid {
		msg.LastSMTPCode = int(lastSMTPCode.Int64)
	}
	if lastSMTPResponse.Valid {
		msg.LastSMTPResponse = lastSMTPResponse.String
	}
	if finalMXHost.Valid {
		msg.FinalMXHost = finalMXHost.String
	}
	if sourceIP.Valid {
		msg.SourceIP = sourceIP.String
	}

	return msg, nil
}

// ExtractDomain extracts domain from email address
func ExtractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[1])
}

// GetMXCache retrieves MX cache data
func (q *Queue) GetMXCache(domain string) (recordsJSON, cachedAt string, ttlSeconds int, err error) {
	query := `
		SELECT mx_records, cached_at, ttl_seconds
		FROM mx_cache
		WHERE domain = ?
	`
	err = q.db.QueryRow(query, domain).Scan(&recordsJSON, &cachedAt, &ttlSeconds)
	return
}

// StoreMXCache stores MX records in cache
func (q *Queue) StoreMXCache(domain string, recordsJSON string, ttlSeconds int) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		INSERT OR REPLACE INTO mx_cache (domain, mx_records, cached_at, ttl_seconds)
		VALUES (?, ?, CURRENT_TIMESTAMP, ?)
	`
	_, err := q.db.Exec(query, domain, recordsJSON, ttlSeconds)
	return err
}

// InvalidateMXCache removes a domain from MX cache
func (q *Queue) InvalidateMXCache(domain string) error {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	_, err := q.db.Exec("DELETE FROM mx_cache WHERE domain = ?", domain)
	return err
}

// CleanupExpiredMXCache removes expired MX cache entries
func (q *Queue) CleanupExpiredMXCache() (int64, error) {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	query := `
		DELETE FROM mx_cache
		WHERE datetime(cached_at, '+' || ttl_seconds || ' seconds') < datetime('now')
	`
	result, err := q.db.Exec(query)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CleanupTerminalMessages removes terminal state messages based on idempotency configuration
func (q *Queue) CleanupTerminalMessages(idempotencyTTLHours int) (int64, error) {
	q.writeMu.Lock()
	defer q.writeMu.Unlock()

	// Two cleanup strategies:
	// 1. Messages WITH idempotency_key: Keep for TTL duration (for deduplication)
	// 2. Messages WITHOUT idempotency_key: Delete immediately (no deduplication needed)

	var query string
	if idempotencyTTLHours <= 0 {
		// TTL=0 means delete all terminal messages immediately (regardless of idempotency key)
		query = `
			DELETE FROM messages
			WHERE status IN ('delivered', 'hard_bounce', 'temp_expired', 'expired')
		`
	} else {
		// Normal cleanup with TTL consideration
		query = `
			DELETE FROM messages
			WHERE status IN ('delivered', 'hard_bounce', 'temp_expired', 'expired')
			  AND (
			    -- Delete immediately if no idempotency key
			    idempotency_key IS NULL
			    OR
			    -- Delete after TTL if has idempotency key
			    (idempotency_key IS NOT NULL AND datetime(created_at, '+' || ? || ' hours') < datetime('now'))
			  )
		`
	}

	var result sql.Result
	var err error
	if idempotencyTTLHours <= 0 {
		result, err = q.db.Exec(query)
	} else {
		result, err = q.db.Exec(query, idempotencyTTLHours)
	}

	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// NotifyChan returns the channel used to notify workers of new messages
func (q *Queue) NotifyChan() <-chan struct{} {
	return q.notifyCh
}

// CallbackNotifyChan returns the channel used to notify callback processor
func (q *Queue) CallbackNotifyChan() <-chan struct{} {
	return q.callbackNotifyCh
}

// SetMetrics sets the metrics recorder for the queue
func (q *Queue) SetMetrics(metrics MetricsRecorder) {
	q.metrics = metrics
}

// UpdateQueueMetrics updates all queue depth metrics
func (q *Queue) UpdateQueueMetrics() error {
	if q.metrics == nil {
		return nil
	}

	// Query counts for each status
	statuses := []MessageStatus{StatusQueued, StatusSending, StatusDelivered, StatusHardBounce, StatusTempExpired, StatusExpired}

	for _, status := range statuses {
		var count int64
		err := q.db.QueryRow("SELECT COUNT(*) FROM messages WHERE status = ?", string(status)).Scan(&count)
		if err != nil {
			return fmt.Errorf("failed to query %s count: %w", status, err)
		}
		q.metrics.RecordQueueDepth(string(status), count)
	}

	return nil
}

// Close closes the database connection
func (q *Queue) Close() error {
	close(q.notifyCh)
	close(q.callbackNotifyCh)
	return q.db.Close()
}
