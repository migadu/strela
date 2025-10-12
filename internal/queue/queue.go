/*
Package queue provides a SQLite-backed message queue with WAL mode for concurrent access.

The queue manages the lifecycle of email messages from submission to delivery,
including retry scheduling, callback management, and delivery attempt auditing.

Message Lifecycle:
  - queued: Initial state after submission
  - sending: Message is being delivered
  - delivered: Successfully delivered to recipient MX
  - hard_bounce: Permanent failure (5xx SMTP codes)
  - temp_expired: Temporary failures exceeded max age
  - expired: Message exceeded max age without delivery

Features:
  - Channel-based worker notifications (30s fallback polling)
  - SQLite WAL mode for concurrent reads during writes
  - Idempotency key support to prevent duplicate submissions
  - Delivery attempt audit trail with SMTP response codes
  - Callback queue for webhook notifications
  - MX record caching with configurable TTL
  - IP reputation tracking
*/
package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// MetricsRecorder is an interface for recording queue-related metrics.
// Implementations typically export data to monitoring systems like Prometheus.
type MetricsRecorder interface {
	// RecordQueueDepth records the number of messages in a specific status.
	RecordQueueDepth(status string, count int64)
}

// Queue manages the persistent message queue using SQLite with WAL mode.
//
// It provides thread-safe operations for enqueueing, dequeueing, and updating
// message status. Worker notification is event-driven via channels, with
// periodic polling as a fallback.
//
// All write operations are serialized via writeMu to ensure SQLite consistency.
type Queue struct {
	db               *sql.DB
	logger           *slog.Logger
	notifyCh         chan struct{}   // Channel to notify workers of new messages
	callbackNotifyCh chan struct{}   // Channel to notify callback processor of new callbacks
	writeMu          sync.Mutex      // Mutex to serialize write operations
	metrics          MetricsRecorder // Optional metrics recorder
}

// QueuedMessage represents a single email message in the queue.
//
// It includes the message content, DKIM signing parameters, delivery state,
// retry scheduling information, and the results of previous delivery attempts.
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

// MessageStatus represents the current state of a message in the queue.
//
// Messages progress through states from queued → sending → delivered/bounce/expired.
// Terminal states are: delivered, hard_bounce, temp_expired, expired.
type MessageStatus string

const (
	StatusQueued      MessageStatus = "queued"
	StatusSending     MessageStatus = "sending"
	StatusDelivered   MessageStatus = "delivered"
	StatusHardBounce  MessageStatus = "hard_bounce"
	StatusTempExpired MessageStatus = "temp_expired"
	StatusExpired     MessageStatus = "expired"
)

// DeliveryAttempt records details of a single delivery attempt for auditing.
//
// This provides a complete audit trail including SMTP response codes,
// error messages, timing information, and error classification.
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

// NewQueue creates a new Queue instance with SQLite WAL mode enabled.
//
// The database connection is configured with:
//   - WAL (Write-Ahead Logging) journal mode for concurrent access
//   - 5 second busy timeout to handle write contention
//   - Automatic schema initialization and migrations
//
// Returns an error if the database cannot be opened or schema initialization fails.
func NewQueue(dbPath string, logger *slog.Logger) (*Queue, error) {
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

	CREATE TABLE IF NOT EXISTS ip_reputation (
		source_ip TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		failure_count INTEGER DEFAULT 0,
		degraded_at TIMESTAMP,
		last_attempt_at TIMESTAMP,
		smtp_code INTEGER,
		smtp_response TEXT,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_ip_status ON ip_reputation(status);
	CREATE INDEX IF NOT EXISTS idx_ip_degraded_at ON ip_reputation(degraded_at);
	`

	_, err := q.db.Exec(schema)
	return err
}

// Enqueue adds a new message to the queue with immediate retry scheduling.
//
// The message is inserted with status 'queued' and next_retry_at set to now,
// making it immediately available for worker processing. Workers are notified
// via the notifyCh channel for instant pickup.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database insert fails (e.g., duplicate message_id).
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
			"message_id", msg.MessageID,
			"error", err)
		return fmt.Errorf("failed to enqueue: %w", err)
	}

	q.logger.Info("message enqueued",
		"message_id", msg.MessageID,
		"to", msg.ToAddr,
		"expires_at", msg.ExpiresAt)

	// Notify workers of new message (non-blocking)
	select {
	case q.notifyCh <- struct{}{}:
	default:
		// Channel full, workers will pick it up on next poll
	}

	return nil
}

// GetNextMessages retrieves a batch of messages ready for processing.
//
// Returns messages with status 'queued' and next_retry_at <= now, ordered by
// next_retry_at (oldest first). The limit parameter controls batch size.
//
// This method is called periodically by workers to fetch messages for delivery.
// It does NOT change message status - workers must call UpdateStatus separately.
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

// UpdateStatus updates the status of a message identified by messageID.
//
// This method changes the message status (e.g., from 'queued' to 'sending',
// or to a terminal state like 'delivered' or 'hard_bounce') and updates the
// updated_at timestamp.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database update fails.
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
			"message_id", messageID,
			"status", string(status),
			"error", err)
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

// ScheduleRetry updates a message for retry after a temporary delivery failure.
//
// This method resets the message status back to 'queued', increments the attempt
// counter, sets the next retry time, and records the error details from the failed
// delivery attempt. The message will be picked up by workers when nextRetry is reached.
//
// Parameters:
//   - messageID: The unique identifier of the message to retry
//   - nextRetry: The time when the message should be retried (based on exponential backoff)
//   - attempts: The total number of delivery attempts made so far
//   - lastError: The error message from the most recent failed attempt
//   - smtpCode: The SMTP response code (e.g., 421 for greylisting, 450 for temporary failure)
//   - smtpResponse: The full SMTP server response message
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database update fails.
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
			"message_id", messageID,
			"next_retry", nextRetry,
			"error", err)
		return fmt.Errorf("failed to schedule retry: %w", err)
	}

	q.logger.Info("retry scheduled",
		"message_id", messageID,
		"attempt", attempts,
		"next_retry", nextRetry)

	return nil
}

// RecordAttempt logs a delivery attempt to the audit trail.
//
// This method creates a permanent record of each delivery attempt, including
// timing information, SMTP response codes, error messages, and error classification.
// The audit trail is stored in the delivery_attempts table and is used for:
//   - Debugging delivery issues
//   - Analyzing delivery patterns
//   - Generating delivery reports
//   - Tracking which MX hosts and source IPs were used
//
// The attempt parameter should include all relevant delivery details including
// whether the attempt succeeded, the SMTP code/response, duration, and error category
// (temporary, permanent, greylist, network).
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database insert fails.
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
			"message_id", attempt.MessageID,
			"error", err)
		return fmt.Errorf("failed to record attempt: %w", err)
	}

	return nil
}

// UpdateDeliveryResult updates the final delivery details for a message.
//
// This method records which MX host successfully accepted the message, which
// source IP was used, and the final SMTP response code and message. This information
// is stored in the main messages table for quick access and reporting.
//
// Typically called after a successful delivery or when a message reaches a terminal
// state (delivered, hard_bounce). The data provides valuable context for callbacks
// and delivery analytics.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database update fails.
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
			"message_id", messageID,
			"error", err)
		return fmt.Errorf("failed to delete message: %w", err)
	}

	q.logger.Debug("message deleted from queue",
		"message_id", messageID)

	return nil
}

// FindExpiredMessages finds all messages that have exceeded their max age.
//
// Returns messages in 'queued' or 'sending' status where expires_at <= now.
// These messages should be marked as 'temp_expired' (if they had temporary failures)
// or 'expired' (if they never had a successful delivery attempt).
//
// The expiration time is set when the message is enqueued and is typically
// calculated as created_at + max_message_age_hours (default: 48 hours).
//
// This method is called periodically by workers to identify messages that have
// been in the queue too long and should no longer be retried.
//
// Returns an error if the database query fails.
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

// EnqueueCallback adds a webhook callback to the callback queue.
//
// This method creates a new callback notification that will be sent to the configured
// webhook URL. Callbacks are queued separately from messages and have their own retry
// logic and circuit breaker to prevent webhook failures from affecting message delivery.
//
// Parameters:
//   - messageID: The unique identifier of the message that triggered the callback
//   - eventType: The type of event (delivered, hard_bounce, temp_expired, expired)
//   - payload: The callback payload (will be JSON-marshaled), typically includes
//     message details, delivery results, SMTP codes, and timestamps
//
// The callback is immediately available for processing (next_retry_at = now).
// The callback processor is notified via callbackNotifyCh for instant pickup.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if JSON marshaling or database insert fails.
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
			"message_id", messageID,
			"event_type", eventType,
			"error", err)
		return fmt.Errorf("failed to enqueue callback: %w", err)
	}

	q.logger.Debug("callback enqueued",
		"message_id", messageID,
		"event_type", eventType)

	// Notify callback processor (non-blocking)
	select {
	case q.callbackNotifyCh <- struct{}{}:
	default:
		// Channel full, processor will pick it up on next poll
	}

	return nil
}

// GetPendingCallbacks retrieves callbacks that are ready to be sent.
//
// Returns callbacks where completed_at is NULL and next_retry_at <= now,
// ordered by next_retry_at (oldest first). The limit parameter controls batch size.
//
// The callback processor calls this method periodically to fetch pending webhooks.
// Unlike message delivery, callbacks that fail after max retries are eventually
// dropped (not kept forever) to prevent the callback queue from growing unbounded.
//
// Returns an error if the database query fails.
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

// MarkCallbackComplete marks a callback as successfully delivered.
//
// This method sets the completed_at timestamp, which excludes the callback from
// future GetPendingCallbacks queries. Completed callbacks remain in the database
// for audit purposes until cleaned up by the periodic cleanup job.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database update fails.
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

// GetMessageByIdempotencyKey retrieves a message using its idempotency key.
//
// Idempotency keys are optional client-provided strings (typically UUIDs) that
// prevent duplicate message submissions. When a client retries a request with
// the same idempotency key, the API returns the original message status instead
// of creating a duplicate.
//
// Returns nil if no message exists with the given idempotency key (allowing the
// submission to proceed). Returns an error only if the database query fails.
//
// The idempotency_key index ensures fast lookups even with large message tables.
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

// GetMXCache retrieves cached MX records for a domain.
//
// Returns the JSON-encoded MX records, the cache timestamp, and the TTL in seconds.
// The caller should check if the cache entry has expired by comparing:
//
//	cached_at + ttl_seconds vs current time
//
// MX record caching reduces DNS queries and improves delivery performance. The cache
// stores both successful lookups (with normal TTL) and failed lookups (with negative TTL)
// to prevent repeated queries for non-existent domains.
//
// Returns sql.ErrNoRows if no cache entry exists for the domain. Returns other errors
// if the database query fails.
func (q *Queue) GetMXCache(domain string) (recordsJSON, cachedAt string, ttlSeconds int, err error) {
	query := `
		SELECT mx_records, cached_at, ttl_seconds
		FROM mx_cache
		WHERE domain = ?
	`
	err = q.db.QueryRow(query, domain).Scan(&recordsJSON, &cachedAt, &ttlSeconds)
	return
}

// StoreMXCache stores MX records in the cache with a specified TTL.
//
// The recordsJSON parameter should contain a JSON-encoded array of MX records
// sorted by priority. The ttlSeconds parameter determines how long the cache
// entry remains valid:
//   - Successful lookups: Use dns.cache_ttl_seconds (default: 3600 = 1 hour)
//   - Failed lookups: Use dns.cache_negative_ttl_seconds (default: 60 = 1 minute)
//
// Uses INSERT OR REPLACE to update existing entries or create new ones. The
// cached_at timestamp is automatically set to CURRENT_TIMESTAMP.
//
// Write operations are serialized via writeMu to ensure SQLite consistency.
// Returns an error if the database operation fails.
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

// GetQueueDepth returns the count of messages in queued and sending status
// This is useful for cluster monitoring via gossip protocol
func (q *Queue) GetQueueDepth() (int64, error) {
	var count int64
	err := q.db.QueryRow("SELECT COUNT(*) FROM messages WHERE status IN ('queued', 'sending')").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to query queue depth: %w", err)
	}
	return count, nil
}

// DatabaseStats holds database statistics
type DatabaseStats struct {
	PageCount       int64   // Total number of pages in database
	PageSize        int64   // Size of each page in bytes
	SizeBytes       int64   // Total database size in bytes
	WALSizeBytes    int64   // Size of WAL file in bytes
	Connections     int     // Number of active connections
	FragmentRatio   float64 // Database fragmentation ratio (0-1)
	CacheHitRatio   float64 // Cache hit ratio (0-1)
	WALCheckpoints  int64   // Number of WAL checkpoints
	QueuedMessages  int64   // Messages in queued status
	SendingMessages int64   // Messages in sending status
}

// GetDatabaseStats retrieves comprehensive database statistics
func (q *Queue) GetDatabaseStats() (*DatabaseStats, error) {
	stats := &DatabaseStats{}

	// Get page count and size
	err := q.db.QueryRow("PRAGMA page_count").Scan(&stats.PageCount)
	if err != nil {
		return nil, fmt.Errorf("failed to get page count: %w", err)
	}

	err = q.db.QueryRow("PRAGMA page_size").Scan(&stats.PageSize)
	if err != nil {
		return nil, fmt.Errorf("failed to get page size: %w", err)
	}

	stats.SizeBytes = stats.PageCount * stats.PageSize

	// Get freelist count (for fragmentation)
	var freelistCount int64
	err = q.db.QueryRow("PRAGMA freelist_count").Scan(&freelistCount)
	if err == nil && stats.PageCount > 0 {
		stats.FragmentRatio = float64(freelistCount) / float64(stats.PageCount)
	}

	// Get cache stats
	var cacheHit, cacheMiss int64
	err = q.db.QueryRow("SELECT cache_hit, cache_miss FROM (SELECT (SELECT value FROM pragma_stats WHERE name='cache_hit') as cache_hit, (SELECT value FROM pragma_stats WHERE name='cache_miss') as cache_miss)").Scan(&cacheHit, &cacheMiss)
	if err == nil && (cacheHit+cacheMiss) > 0 {
		stats.CacheHitRatio = float64(cacheHit) / float64(cacheHit+cacheMiss)
	}

	// Get WAL checkpoint count
	q.db.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&stats.WALCheckpoints, nil, nil)

	// Get queue depth by status
	err = q.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'sending' THEN 1 ELSE 0 END), 0)
		FROM messages
	`).Scan(&stats.QueuedMessages, &stats.SendingMessages)
	if err != nil {
		return nil, fmt.Errorf("failed to get message counts: %w", err)
	}

	// Get connection pool stats from database/sql
	dbStats := q.db.Stats()
	stats.Connections = dbStats.OpenConnections

	return stats, nil
}

// GetDatabasePath returns the path to the database file (if available from connection string)
func (q *Queue) GetDB() *sql.DB {
	return q.db
}

// Close closes the database connection
func (q *Queue) Close() error {
	close(q.notifyCh)
	close(q.callbackNotifyCh)
	return q.db.Close()
}
