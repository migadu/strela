package delivery

import (
	"fmt"
	"strings"
)

// ErrorCategory represents the type of delivery error
type ErrorCategory string

const (
	// ErrorTemporary - retry with backoff (4xx codes, network issues)
	ErrorTemporary ErrorCategory = "temporary"

	// ErrorPermanent - hard bounce, deactivate email (5xx codes)
	ErrorPermanent ErrorCategory = "permanent"

	// ErrorGreylist - aggressive retry for greylisting (421)
	ErrorGreylist ErrorCategory = "greylist"

	// ErrorNetwork - connection/DNS failures, retry
	ErrorNetwork ErrorCategory = "network"

	// ErrorThrottled - rate limit active for destination domain
	ErrorThrottled ErrorCategory = "throttled"

	// ErrorReputation - IP blacklisted or poor reputation
	ErrorReputation ErrorCategory = "reputation"
)

// DeliveryError represents a classified delivery error
type DeliveryError struct {
	Category     ErrorCategory
	SMTPCode     int
	SMTPResponse string
	Message      string
	OriginalErr  error
}

// Error implements error interface
func (e *DeliveryError) Error() string {
	if e.SMTPCode > 0 {
		return fmt.Sprintf("%s error (SMTP %d): %s", e.Category, e.SMTPCode, e.Message)
	}
	return fmt.Sprintf("%s error: %s", e.Category, e.Message)
}

// ClassifyError determines the error category from SMTP response or network error
func ClassifyError(smtpCode int, smtpResponse string, err error) *DeliveryError {
	// Network/connection errors
	if err != nil && smtpCode == 0 {
		return classifyNetworkError(err)
	}

	// SMTP response code classification
	return classifySMTPCode(smtpCode, smtpResponse)
}

// classifyNetworkError categorizes network-level errors
func classifyNetworkError(err error) *DeliveryError {
	errStr := strings.ToLower(err.Error())

	// DNS errors
	if strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "dns") ||
		strings.Contains(errStr, "lookup") {
		return &DeliveryError{
			Category:    ErrorNetwork,
			Message:     fmt.Sprintf("DNS lookup failed: %s", err.Error()),
			OriginalErr: err,
		}
	}

	// Connection errors
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection timeout") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "network") {
		return &DeliveryError{
			Category:    ErrorNetwork,
			Message:     fmt.Sprintf("Network connection failed: %s", err.Error()),
			OriginalErr: err,
		}
	}

	// TLS errors
	if strings.Contains(errStr, "tls") ||
		strings.Contains(errStr, "certificate") ||
		strings.Contains(errStr, "handshake") {
		return &DeliveryError{
			Category:    ErrorNetwork,
			Message:     fmt.Sprintf("TLS error: %s", err.Error()),
			OriginalErr: err,
		}
	}

	// Default to network error for unknown errors
	return &DeliveryError{
		Category:    ErrorNetwork,
		Message:     fmt.Sprintf("Network error: %s", err.Error()),
		OriginalErr: err,
	}
}

// classifySMTPCode categorizes errors based on SMTP response code
func classifySMTPCode(code int, response string) *DeliveryError {
	// Check for reputation issues first
	if isReputationError(response) {
		return &DeliveryError{
			Category:     ErrorReputation,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      "IP reputation/blacklist error",
		}
	}

	switch {
	// 2xx - Success (shouldn't be an error)
	case code >= 200 && code < 300:
		return nil

	// 421 - Greylisting (temporary, but needs aggressive retry)
	case code == 421:
		return &DeliveryError{
			Category:     ErrorGreylist,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      "Greylisting detected",
		}

	// 4xx - Temporary failures
	case code >= 400 && code < 500:
		return &DeliveryError{
			Category:     ErrorTemporary,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      classifyTemporaryError(code, response),
		}

	// 5xx - Permanent failures (hard bounce)
	case code >= 500 && code < 600:
		return &DeliveryError{
			Category:     ErrorPermanent,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      classifyPermanentError(code, response),
		}

	// Unknown/invalid code
	default:
		return &DeliveryError{
			Category:     ErrorNetwork,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      fmt.Sprintf("Unknown SMTP code: %d", code),
		}
	}
}

// isReputationError checks for keywords indicating a reputation issue
func isReputationError(response string) bool {
	responseLower := strings.ToLower(response)
	reputationKeywords := []string{
		"blocked",
		"blacklist",
		"poor reputation",
		"rejected for policy reasons",
		"rbl",
		"dnsbl",
		"spamhaus",
		"proofpoint",
		"cloudmark",
		"barracuda",
		"unfortunately",
	}

	for _, keyword := range reputationKeywords {
		if strings.Contains(responseLower, keyword) {
			return true
		}
	}
	return false
}

// classifyTemporaryError provides detailed categorization of 4xx errors
func classifyTemporaryError(code int, response string) string {
	responseLower := strings.ToLower(response)

	switch code {
	case 421:
		return "Service not available (greylisting or rate limiting)"
	case 450:
		if strings.Contains(responseLower, "rate") || strings.Contains(responseLower, "limit") || strings.Contains(responseLower, "too many") {
			return "Rate limit exceeded"
		}
		return "Mailbox busy or unavailable"
	case 451:
		if strings.Contains(responseLower, "rate") || strings.Contains(responseLower, "limit") {
			return "Rate limit exceeded"
		}
		return "Local processing error"
	case 452:
		return "Insufficient system storage"
	case 454:
		return "TLS negotiation failed"
	default:
		if strings.Contains(responseLower, "quota") {
			return "Mailbox quota exceeded"
		}
		if strings.Contains(responseLower, "rate") {
			return "Rate limiting"
		}
		if strings.Contains(responseLower, "busy") {
			return "Server busy"
		}
		return fmt.Sprintf("Temporary failure (SMTP %d)", code)
	}
}

// classifyPermanentError provides detailed categorization of 5xx errors
func classifyPermanentError(code int, response string) string {
	responseLower := strings.ToLower(response)

	switch code {
	case 550:
		if strings.Contains(responseLower, "user") && (strings.Contains(responseLower, "not found") || strings.Contains(responseLower, "unknown")) {
			return "User not found"
		}
		if strings.Contains(responseLower, "mailbox") && strings.Contains(responseLower, "unavailable") {
			return "Mailbox unavailable"
		}
		if strings.Contains(responseLower, "relay") || strings.Contains(responseLower, "relaying") {
			return "Relaying denied"
		}
		if strings.Contains(responseLower, "spam") || strings.Contains(responseLower, "blocked") {
			return "Message rejected as spam"
		}
		return "Mailbox unavailable or policy rejection"
	case 551:
		return "User not local, try different path"
	case 552:
		return "Message size exceeds limit"
	case 553:
		return "Invalid mailbox name"
	case 554:
		if strings.Contains(responseLower, "spam") {
			return "Rejected as spam"
		}
		if strings.Contains(responseLower, "policy") {
			return "Policy rejection"
		}
		return "Transaction failed"
	default:
		return fmt.Sprintf("Permanent failure (SMTP %d)", code)
	}
}

// ShouldDeactivateEmail determines if an email should be deactivated based on error
func ShouldDeactivateEmail(category ErrorCategory, smtpCode int, response string) bool {
	if category != ErrorPermanent {
		return false
	}

	responseLower := strings.ToLower(response)

	// Deactivate for user not found / mailbox does not exist
	if smtpCode == 550 {
		if strings.Contains(responseLower, "user") && (strings.Contains(responseLower, "not found") || strings.Contains(responseLower, "unknown")) {
			return true
		}
		if strings.Contains(responseLower, "mailbox") && (strings.Contains(responseLower, "not found") || strings.Contains(responseLower, "does not exist")) {
			return true
		}
		if strings.Contains(responseLower, "recipient") && (strings.Contains(responseLower, "not found") || strings.Contains(responseLower, "unknown")) {
			return true
		}
	}

	// Deactivate for invalid mailbox name
	if smtpCode == 553 {
		return true
	}

	// Don't deactivate for:
	// - Spam/policy rejections (might be temporary)
	// - Size limits (message-specific)
	// - Relay issues (configuration issue)
	return false
}

// IsRetryable determines if an error should be retried
func IsRetryable(category ErrorCategory) bool {
	switch category {
	case ErrorTemporary, ErrorGreylist, ErrorNetwork, ErrorThrottled, ErrorReputation:
		return true
	case ErrorPermanent:
		return false
	default:
		return false
	}
}
