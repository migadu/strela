package handler

// HTTP response structures for the queue-based API

// EnqueueResponse is returned when a message is accepted into the queue
type EnqueueResponse struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"` // "queued"
	QueuedAt  string `json:"queued_at"`
}

// DeliveryEventCallback is the payload sent to CloudFlare Worker
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

// MessageRequest represents incoming HTTP message request
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
