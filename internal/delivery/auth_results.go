package delivery

// InboundAuthResults carries authentication results reported by the caller for
// the original incoming message. This is only relevant for forwarding scenarios
// where the message was scanned before being submitted to strela for re-delivery.
// For normal outbound sends this should be nil.
type InboundAuthResults struct {
	DKIM  string `json:"dkim,omitempty"`  // e.g. "pass", "fail", "none"
	SPF   string `json:"spf,omitempty"`   // e.g. "pass", "fail", "softfail", "none"
	DMARC string `json:"dmarc,omitempty"` // e.g. "pass", "fail", "none"
}
