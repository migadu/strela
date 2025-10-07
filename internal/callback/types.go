// Package callback implements webhook notifications for email delivery events.
//
// The callback package provides asynchronous HTTP webhook notifications to external
// services when delivery events occur (successful delivery, bounces, expirations).
// Callbacks are queued in a separate SQLite table and processed by dedicated workers
// with retry logic, exponential backoff, and circuit breaker protection.
//
// # Supported Events
//
// The callback system sends notifications for these delivery events:
//   - delivered: Message successfully delivered to recipient MX server
//   - hard_bounce: Permanent delivery failure (5xx SMTP response)
//   - temp_expired: Message expired after exhausting temporary failure retries
//   - expired: Message expired without successful delivery
//
// # Architecture
//
// Callbacks are processed asynchronously:
//  1. Delivery worker enqueues callback when message reaches terminal state
//  2. Callback worker retrieves pending callbacks from queue
//  3. HTTP POST request sent to configured webhook URL
//  4. On failure: exponential backoff retry up to max age (default 24h)
//  5. Circuit breaker prevents cascading failures from unreachable webhooks
//
// # Retry Strategy
//
// Failed callbacks are retried with exponential backoff:
//   - Initial delay: 30 seconds (configurable)
//   - Backoff multiplier: 2.0 (configurable)
//   - Maximum delay: 1 hour (configurable)
//   - Maximum age: 24 hours (configurable)
//
// Unlike message delivery (which is attempt-based), callback retry is age-based.
// Callbacks are abandoned after exceeding max age, regardless of attempt count.
//
// # Circuit Breaker
//
// The callback circuit breaker protects against webhook failures:
//   - Opens after N consecutive network/timeout errors (default: 5)
//   - Remains open for configured timeout (default: 5 minutes)
//   - Transitions to half-open to test recovery
//   - Closes after M consecutive successes (default: 2)
//
// When open, callbacks are postponed (not lost) and rescheduled after the timeout.
// HTTP 5xx errors from the webhook endpoint do NOT trigger the circuit breaker,
// as they indicate application errors rather than infrastructure failures.
//
// # Graceful Shutdown
//
// The callback processor supports graceful shutdown. In-flight HTTP requests are
// allowed to complete before the processor exits. Pending callbacks remain in the
// queue and are processed on restart.
package callback

// DeliveryEventCallback is the JSON payload sent to webhook endpoints for delivery events.
//
// This structure contains comprehensive information about the email delivery outcome,
// including message metadata, delivery details, and error information for failures.
// The Event field determines which fields are populated.
type DeliveryEventCallback struct {
	// MessageID is the unique identifier for the message (UUID format).
	MessageID string `json:"message_id"`

	// Event indicates the delivery outcome type.
	// Valid values: "delivered", "hard_bounce", "temp_expired", "expired"
	Event string `json:"event"`

	// Email is the recipient address (to_addr from original submission).
	Email string `json:"email"`

	// From is the sender address (from_addr from original submission).
	From string `json:"from"`

	// Subject is the email subject line.
	Subject string `json:"subject"`

	// DeliveredAt is the timestamp of the event in ISO 8601 format (RFC3339).
	// Present for all event types.
	DeliveredAt string `json:"delivered_at,omitempty"`

	// Attempts is the total number of delivery attempts made.
	// For "delivered" and "hard_bounce": includes the final attempt.
	// For "temp_expired": total attempts before giving up.
	// For "expired": attempts made before expiration.
	Attempts int `json:"attempts"`

	// SMTPCode is the final SMTP response code from the recipient server.
	// Present for "delivered" and "hard_bounce" events.
	// Examples: 250 (success), 550 (mailbox unavailable), 554 (transaction failed).
	SMTPCode int `json:"smtp_code,omitempty"`

	// SMTPResponse is the full SMTP response message from the recipient server.
	// Present for "delivered" and "hard_bounce" events.
	SMTPResponse string `json:"smtp_response,omitempty"`

	// FinalMXHost is the MX hostname that accepted or rejected the message.
	// Present for "delivered" and "hard_bounce" events when SMTP connection succeeded.
	FinalMXHost string `json:"final_mx_host,omitempty"`

	// SourceIP is the source IP address used for the delivery attempt.
	// Present for "delivered" and "hard_bounce" events when connection succeeded.
	SourceIP string `json:"source_ip,omitempty"`

	// Reason provides additional failure details for unsuccessful deliveries.
	// Present for "hard_bounce", "temp_expired", and "expired" events.
	// Examples: "permanent_bounce", "temporary_failure_exhausted", "delivery_timeout".
	Reason string `json:"reason,omitempty"`
}
