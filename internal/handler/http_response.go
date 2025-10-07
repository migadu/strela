package handler

// HTTP response structures for the queue-based API

// EnqueueResponse is returned when a message is successfully accepted into the queue.
//
// This response is sent immediately after queueing (HTTP 202 Accepted), making the API
// fully asynchronous. The actual delivery happens in background workers, with results
// communicated via webhook callbacks.
//
// The Status field is always "queued" for new messages. For idempotent requests that
// match an existing message, Status reflects the current state of that message
// (e.g., "queued", "sending", "delivered").
//
// Example Response:
//
//	HTTP/1.1 202 Accepted
//	Content-Type: application/json
//
//	{
//	    "message_id": "20251007-123456-abc123def456",
//	    "status": "queued",
//	    "queued_at": "2025-10-07T12:34:56Z"
//	}
type EnqueueResponse struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"` // "queued"
	QueuedAt  string `json:"queued_at"`
}

// DeliveryEventCallback is the webhook payload sent for delivery events.
//
// This structure represents the JSON payload sent to the configured webhook URL
// (e.g., CloudFlare Worker, AWS Lambda, custom HTTP endpoint) when a message
// reaches a terminal state.
//
// Terminal States and Events:
//   - "delivered": Message successfully delivered to recipient MX server
//   - "hard_bounce": Permanent failure (5xx SMTP code, recipient doesn't exist, etc.)
//   - "temp_expired": Temporary failure exceeded max retry time
//   - "expired": Message expired before delivery could be attempted
//
// The webhook includes detailed information about the delivery attempt:
//   - SMTP response code and message
//   - Final MX host that accepted/rejected the message
//   - Source IP used for delivery
//   - Number of delivery attempts
//   - Reason for failure (if applicable)
//
// Example Webhook Payload (delivered):
//
//	POST https://webhook.example.com/delivery-events
//	Content-Type: application/json
//
//	{
//	    "message_id": "20251007-123456-abc123def456",
//	    "event": "delivered",
//	    "email": "recipient@example.com",
//	    "from": "sender@mycompany.com",
//	    "subject": "Welcome Email",
//	    "delivered_at": "2025-10-07T12:35:23Z",
//	    "attempts": 1,
//	    "smtp_code": 250,
//	    "smtp_response": "250 2.0.0 OK: queued as ABC123",
//	    "final_mx_host": "mx1.example.com",
//	    "source_ip": "203.0.113.1"
//	}
//
// Example Webhook Payload (hard_bounce):
//
//	{
//	    "message_id": "20251007-123456-abc123def456",
//	    "event": "hard_bounce",
//	    "email": "nonexistent@example.com",
//	    "from": "sender@mycompany.com",
//	    "subject": "Welcome Email",
//	    "attempts": 1,
//	    "smtp_code": 550,
//	    "smtp_response": "550 5.1.1 User unknown",
//	    "final_mx_host": "mx1.example.com",
//	    "source_ip": "203.0.113.1",
//	    "reason": "Recipient address rejected: User unknown"
//	}
type DeliveryEventCallback struct {
	MessageID string `json:"message_id"`
	Event     string `json:"event"` // "delivered", "hard_bounce", "temp_expired", "expired"

	// Message details
	Email   string `json:"email"` // to_addr
	From    string `json:"from"`  // from_addr
	Subject string `json:"subject"`

	// Timing
	DeliveredAt string `json:"delivered_at,omitempty"` // ISO 8601
	Attempts    int    `json:"attempts"`

	// Delivery details (for delivered/hard_bounce)
	SMTPCode     int    `json:"smtp_code,omitempty"`
	SMTPResponse string `json:"smtp_response,omitempty"`
	FinalMXHost  string `json:"final_mx_host,omitempty"`
	SourceIP     string `json:"source_ip,omitempty"`

	// Failure reason (for hard_bounce/temp_expired/expired)
	Reason string `json:"reason,omitempty"`
}

// MessageRequest represents an incoming HTTP message submission request.
//
// This structure supports both JSON and form-encoded content types:
//   - application/json: All fields as JSON properties
//   - application/x-www-form-urlencoded: All fields as form parameters
//
// Required Fields:
//   - From: Sender email address (e.g., "sender@example.com")
//   - To: Recipient email address (single recipient only)
//   - Subject: Email subject line
//   - Text OR HTML: At least one body format must be provided
//
// Optional Fields:
//   - Text: Plain text body (recommended for compatibility)
//   - HTML: HTML body (for rich formatting)
//   - DKIMPrivateKey: PEM-encoded RSA private key for DKIM signing
//   - DKIMSelector: DKIM selector (e.g., "default", "mail")
//   - DKIMDomain: Domain for DKIM signature (defaults to sender's domain)
//
// Body Formats:
//   - If only Text is provided: Message is sent as text/plain
//   - If only HTML is provided: Message is sent as text/html
//   - If both are provided: Message is sent as multipart/alternative (both formats)
//
// DKIM Signing:
//
// To enable DKIM signing, provide DKIMPrivateKey and DKIMSelector:
//
//	{
//	    "from": "sender@example.com",
//	    "to": "recipient@example.com",
//	    "subject": "Test",
//	    "text": "Hello World",
//	    "dkim_private_key": "-----BEGIN RSA PRIVATE KEY-----\n...",
//	    "dkim_selector": "default"
//	}
//
// The DKIMDomain defaults to the domain of the From address if not specified.
//
// Example JSON Request:
//
//	POST /v1/messages HTTP/1.1
//	Content-Type: application/json
//	Authorization: Bearer your-token-here
//
//	{
//	    "from": "sender@mycompany.com",
//	    "to": "recipient@example.com",
//	    "subject": "Welcome!",
//	    "text": "Welcome to our service!",
//	    "html": "<h1>Welcome to our service!</h1>"
//	}
//
// Example Form-Encoded Request:
//
//	POST /v1/messages HTTP/1.1
//	Content-Type: application/x-www-form-urlencoded
//	Authorization: Bearer your-token-here
//
//	from=sender@mycompany.com&to=recipient@example.com&subject=Welcome&text=Hello
type MessageRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`

	// DKIM signing (optional)
	DKIMPrivateKey string `json:"dkim_private_key,omitempty"` // PEM-encoded RSA private key
	DKIMSelector   string `json:"dkim_selector,omitempty"`    // DKIM selector (e.g., "default")
	DKIMDomain     string `json:"dkim_domain,omitempty"`      // Domain for DKIM (defaults to From domain)
}
