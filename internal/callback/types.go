package callback

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
