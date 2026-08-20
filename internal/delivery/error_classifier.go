package delivery

import (
	"fmt"
	"strings"
)

// ErrorCategory represents the type of delivery error and determines retry behavior.
type ErrorCategory string

const (
	// ErrorTemporary indicates a temporary failure that should be retried with exponential backoff.
	// Includes 4xx SMTP codes and some network issues. Examples: mailbox full, rate limits.
	ErrorTemporary ErrorCategory = "temporary"

	// ErrorPermanent indicates a permanent failure that should not be retried.
	// Includes 5xx SMTP codes. Examples: user not found, invalid mailbox name, spam rejection.
	ErrorPermanent ErrorCategory = "permanent"

	// ErrorGreylist indicates greylisting, detected from the response text of a
	// 4xx reply (e.g. "451 4.2.0 Greylisted, please try again later"). In v2.0
	// it behaves exactly like ErrorTemporary (status "temp_fail", retryable);
	// it remains a distinct category only for the caller-visible error message
	// and debug logs.
	ErrorGreylist ErrorCategory = "greylist"

	// ErrorNetwork indicates connection or DNS failures that should be retried.
	// Examples: connection refused, DNS lookup failed, TLS errors, timeouts.
	ErrorNetwork ErrorCategory = "network"

	// ErrorThrottled indicates our per-domain rate limit is active.
	// The message will be retried after the throttle interval expires.
	ErrorThrottled ErrorCategory = "throttled"

	// ErrorReputation indicates the source IP is blacklisted or has poor reputation.
	// The IP will be marked as degraded and removed from rotation.
	ErrorReputation ErrorCategory = "reputation"
)

// DeliveryError represents a classified delivery error with categorization,
// SMTP codes, and the original error. The category determines retry behavior
// and whether the IP reputation should be affected.
type DeliveryError struct {
	Category     ErrorCategory
	SMTPCode     int
	SMTPResponse string
	Message      string
	OriginalErr  error
	// ImmediateDegrade is set only for ErrorReputation. When true, the match came
	// from a strong, unambiguous listing keyword (e.g. Spamhaus/DNSBL) and the
	// source IP should be degraded on the first hit. When false, the match came
	// from a tightened weak keyword and degradation is threshold-gated by the
	// reputation tracker (corroboration required).
	ImmediateDegrade bool
}

// Error implements error interface
func (e *DeliveryError) Error() string {
	if e.SMTPCode > 0 {
		return fmt.Sprintf("%s error (SMTP %d): %s", e.Category, e.SMTPCode, e.Message)
	}
	return fmt.Sprintf("%s error: %s", e.Category, e.Message)
}

// Unwrap returns the original error, enabling errors.Is() and errors.As() chains.
func (e *DeliveryError) Unwrap() error {
	return e.OriginalErr
}

// ClassifyError determines the error category from SMTP response codes or network errors.
// It first checks for network-level errors (DNS, connection failures), then examines SMTP
// response codes and messages to classify the error. Reputation errors are detected by
// scanning for blacklist-related keywords in the SMTP response.
// sourceIP is the local source IP used for the attempt (may be ""); some
// providers quote it in IP-reputation rejections, which is a strong IP-scoped
// signal (see hasIPScopedSignal).
func ClassifyError(smtpCode int, smtpResponse string, sourceIP string, err error) *DeliveryError {
	// Network/connection errors
	if err != nil && smtpCode == 0 {
		return classifyNetworkError(err)
	}

	// SMTP response code classification
	return classifySMTPCode(smtpCode, smtpResponse, sourceIP)
}

// classifyNetworkError categorizes network-level errors
func classifyNetworkError(err error) *DeliveryError {
	errStr := strings.ToLower(err.Error())

	// Timeouts (Context or Network)
	if strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "timeout") {
		return &DeliveryError{
			Category:    ErrorNetwork,
			Message:     fmt.Sprintf("Timeout exceeded: %s", err.Error()),
			OriginalErr: err,
		}
	}

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
func classifySMTPCode(code int, response string, sourceIP string) *DeliveryError {
	// 2xx - Success (shouldn't be an error). Must be checked before the
	// keyword scans below so a success response can never be misclassified.
	if code >= 200 && code < 300 {
		return nil
	}

	// Check for reputation issues
	if isRep, strong := isReputationError(code, response, sourceIP); isRep {
		return &DeliveryError{
			Category:         ErrorReputation,
			SMTPCode:         code,
			SMTPResponse:     response,
			Message:          "IP reputation/blacklist error",
			ImmediateDegrade: strong,
		}
	}

	// Greylisting is identified by response text, not code: real greylisters
	// mostly answer 450/451 and say so, while plain 421s are usually rate
	// limiting or load shedding.
	if code >= 400 && code < 500 && isGreylistResponse(response) {
		return &DeliveryError{
			Category:     ErrorGreylist,
			SMTPCode:     code,
			SMTPResponse: response,
			Message:      "Greylisting detected",
		}
	}

	switch {
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

// isReputationError checks for keywords indicating a reputation issue.
//
// It returns (isRep, strong):
//   - strong == true  → an explicit blocklist/reputation reference. These match
//     at any code (blocklist operators deliver listings via 4xx as well as 5xx)
//     and should degrade the IP immediately.
//   - strong == false → a tightened weak keyword. Weak keywords also appear in
//     per-message content rejections (e.g. Gmail "this message has been
//     blocked"), so they count as reputation only on a definitive 5xx AND when
//     the response carries an IP-scoped signal (hasIPScopedSignal). Degradation
//     for these is threshold-gated by the reputation tracker.
func isReputationError(code int, response string, sourceIP string) (isRep bool, strong bool) {
	responseLower := strings.ToLower(response)

	strongKeywords := []string{
		"blacklist",
		"poor reputation",
		"rbl",
		"dnsbl",
		"spamhaus",
		"proofpoint",
		"cloudmark",
		"barracuda",
	}
	for _, keyword := range strongKeywords {
		if strings.Contains(responseLower, keyword) {
			return true, true
		}
	}

	if code >= 500 && code < 600 {
		weakKeywords := []string{
			"blocked",
			"rejected for policy reasons",
		}
		for _, keyword := range weakKeywords {
			if strings.Contains(responseLower, keyword) && hasIPScopedSignal(responseLower, sourceIP) {
				return true, false
			}
		}
	}
	return false, false
}

// hasIPScopedSignal reports whether the (already lower-cased) response text
// points at the sending IP rather than at the individual message. Weak
// reputation keywords only count as an IP-reputation event when accompanied by
// such a signal; otherwise a per-message content block (Gmail's "this message
// has been blocked") would wrongly degrade the whole source IP for every
// destination.
//
// There is deliberately NO message-scoped veto: Gmail's genuine IP-reputation
// block (S3140) contains both "your ip address" and "unsolicited", so vetoing on
// message words would suppress the one Gmail response that IS an IP event. The
// per-message content block has no IP language and simply fails this check.
func hasIPScopedSignal(responseLower, sourceIP string) bool {
	// Some providers quote the exact sending IP, e.g. Outlook S3150:
	// "banned sending IP [192.0.2.1]". That is an unambiguous IP-scoped signal.
	if sourceIP != "" && strings.Contains(responseLower, strings.ToLower(sourceIP)) {
		return true
	}

	ipSignals := []string{
		"your ip",
		"ip address", // also covers "the ip address"
		"sending ip",
		// Listing phrasing, kept specific so it does not match "whitelisted",
		// "allowlisted", "greylisted", or "delisted" (all contain "listed").
		"is listed",
		"listed on",
		"listed in",
		"listed by",
		"reputation",
		"ptr",
		"rdns",
		"reverse dns",
		"blocklist",
		"blacklist",
	}
	for _, sig := range ipSignals {
		if strings.Contains(responseLower, sig) {
			return true
		}
	}
	return false
}

// isGreylistResponse checks whether a response text indicates greylisting.
func isGreylistResponse(response string) bool {
	responseLower := strings.ToLower(response)
	greylistKeywords := []string{
		"greylist",
		"graylist",
		"grey-list",
		"gray-list",
		"grey listed",
		"gray listed",
	}
	for _, keyword := range greylistKeywords {
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
		// Greylist-texted responses are caught earlier in classifySMTPCode;
		// a plain 421 is almost always rate limiting or load shedding.
		return "Service not available (rate limiting or server shutdown)"
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
		// RFC 5321 uses 452 for insufficient storage, but it is also commonly
		// used for recipient-count limits (e.g. Postfix "too many recipients").
		if strings.Contains(responseLower, "recipient") {
			return "Too many recipients"
		}
		if strings.Contains(responseLower, "rate") || strings.Contains(responseLower, "limit") {
			return "Rate limit exceeded"
		}
		return "Insufficient system storage"
	case 454:
		// RFC 3207 uses 454 for "TLS not available", but Postfix also uses it
		// for deferred relay denial (defer_unauth_destination).
		if strings.Contains(responseLower, "tls") || strings.Contains(responseLower, "starttls") {
			return "TLS negotiation failed"
		}
		if strings.Contains(responseLower, "relay") {
			return "Relay access denied (deferred)"
		}
		return "Temporary failure (SMTP 454)"
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
		// Sendmail-style servers use 553 for relay denial, not just bad mailbox names.
		if strings.Contains(responseLower, "relay") {
			return "Relaying denied"
		}
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

// ShouldDeactivateEmail determines if an email address should be deactivated based on the error.
// Returns true only for permanent errors indicating the user or mailbox does not exist
// (SMTP 550 with "user not found"/"mailbox not found" messages, or SMTP 553 invalid mailbox).
// Does not deactivate for spam rejections, size limits, or relay issues, as these may be temporary.
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

	// Deactivate for invalid mailbox name — but not for relay denials,
	// which are remote configuration issues, not bad addresses
	if smtpCode == 553 && !strings.Contains(responseLower, "relay") {
		return true
	}

	// Don't deactivate for:
	// - Spam/policy rejections (might be temporary)
	// - Size limits (message-specific)
	// - Relay issues (configuration issue)
	return false
}

// IsRetryable determines if an error category should trigger retry attempts.
// Returns true for temporary, greylist, network, throttled, and reputation errors.
// Returns false for permanent errors, which represent hard bounces.
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
