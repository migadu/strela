package delivery

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyError_Greylist(t *testing.T) {
	// Greylisting is detected from response text on any 4xx code, since real
	// greylisters mostly answer 450/451 and say so.
	tests := []struct {
		code     int
		response string
	}{
		{421, "4.7.1 Greylisted, please try again later"},
		{450, "4.2.0 Greylisted, see http://postgrey.schweikert.ch/help"},
		{451, "4.7.1 Graylisting in action, please come back later"},
		{451, "4.2.0 You have been grey listed, retry in 300 seconds"},
	}

	for _, tt := range tests {
		err := ClassifyError(tt.code, tt.response, "", nil)

		if err.Category != ErrorGreylist {
			t.Errorf("Code %d (%q): expected category %s, got %s", tt.code, tt.response, ErrorGreylist, err.Category)
		}

		if err.SMTPCode != tt.code {
			t.Errorf("Code %d: expected SMTP code %d, got %d", tt.code, tt.code, err.SMTPCode)
		}
	}
}

func TestClassifyError_Plain421IsNotGreylist(t *testing.T) {
	// A 421 without greylist text is rate limiting or load shedding
	// (e.g. Gmail's throttling), not greylisting.
	err := ClassifyError(421, "4.7.0 Try again later, closing connection", "", nil)

	if err.Category != ErrorTemporary {
		t.Errorf("Expected category %s, got %s", ErrorTemporary, err.Category)
	}

	if err.Message != "Service not available (rate limiting or server shutdown)" {
		t.Errorf("Unexpected message: %q", err.Message)
	}
}

func TestClassifyError_TemporaryCodes(t *testing.T) {
	tests := []struct {
		code     int
		response string
		expected string
	}{
		{450, "Mailbox busy", "Mailbox busy or unavailable"},
		{451, "Rate limit exceeded", "Rate limit exceeded"},
		{452, "Insufficient storage", "Insufficient system storage"},
		{452, "4.5.3 Error: too many recipients", "Too many recipients"},
		{452, "4.7.0 Rate limit reached, try later", "Rate limit exceeded"},
		{454, "TLS failed", "TLS negotiation failed"},
		{454, "4.7.0 TLS not available due to local problem", "TLS negotiation failed"},
		{454, "<user@example.com>: Relay access denied", "Relay access denied (deferred)"},
		{454, "4.3.0 Try again later", "Temporary failure (SMTP 454)"},
	}

	for _, tt := range tests {
		err := ClassifyError(tt.code, tt.response, "", nil)

		if err.Category != ErrorTemporary {
			t.Errorf("Code %d: expected category %s, got %s", tt.code, ErrorTemporary, err.Category)
		}

		if err.SMTPCode != tt.code {
			t.Errorf("Code %d: expected SMTP code %d, got %d", tt.code, tt.code, err.SMTPCode)
		}

		if err.Message != tt.expected {
			t.Errorf("Code %d (%q): expected message %q, got %q", tt.code, tt.response, tt.expected, err.Message)
		}
	}
}

func TestClassifyError_PermanentCodes(t *testing.T) {
	tests := []struct {
		code     int
		response string
		expected ErrorCategory
	}{
		{550, "User not found", ErrorPermanent},
		{551, "User not local", ErrorPermanent},
		{552, "Message too large", ErrorPermanent},
		{553, "Invalid mailbox", ErrorPermanent},
		{554, "Transaction failed", ErrorPermanent},
	}

	for _, tt := range tests {
		err := ClassifyError(tt.code, tt.response, "", nil)

		if err.Category != tt.expected {
			t.Errorf("Code %d: expected category %s, got %s", tt.code, tt.expected, err.Category)
		}

		if err.SMTPCode != tt.code {
			t.Errorf("Code %d: expected SMTP code %d, got %d", tt.code, tt.code, err.SMTPCode)
		}
	}
}

func TestClassifyError_NetworkErrors(t *testing.T) {
	tests := []struct {
		err      error
		expected ErrorCategory
	}{
		{errors.New("dial tcp: lookup example.com: no such host"), ErrorNetwork},
		{errors.New("connection refused"), ErrorNetwork},
		{errors.New("connection reset by peer"), ErrorNetwork},
		{errors.New("i/o timeout"), ErrorNetwork},
		{errors.New("TLS handshake failed"), ErrorNetwork},
		{errors.New("x509: certificate has expired"), ErrorNetwork},
	}

	for _, tt := range tests {
		err := ClassifyError(0, "", "", tt.err)

		if err.Category != tt.expected {
			t.Errorf("Error '%s': expected category %s, got %s", tt.err, tt.expected, err.Category)
		}

		if err.OriginalErr != tt.err {
			t.Errorf("Error '%s': original error not preserved", tt.err)
		}
	}
}

func TestClassifyError_SuccessCodes(t *testing.T) {
	// 2xx codes should return nil (not an error)
	err := ClassifyError(250, "OK", "", nil)
	if err != nil {
		t.Errorf("Expected nil for success code 250, got %v", err)
	}

	err = ClassifyError(220, "Service ready", "", nil)
	if err != nil {
		t.Errorf("Expected nil for success code 220, got %v", err)
	}

	// A 2xx must return nil even if the text happens to contain a
	// reputation keyword (success check runs before keyword scans).
	err = ClassifyError(250, "2.0.0 OK: message no longer blocked", "", nil)
	if err != nil {
		t.Errorf("Expected nil for success code 250 with keyword text, got %v", err)
	}
}

func TestClassifyError_Reputation(t *testing.T) {
	tests := []struct {
		code      int
		response  string
		sourceIP  string
		expected  ErrorCategory
		immediate bool // only meaningful when expected == ErrorReputation
	}{
		// Strong keywords match at any code — blocklist operators deliver
		// listings via 4xx as well as 5xx — and degrade immediately.
		{451, "4.7.1 Service unavailable, client host listed on Spamhaus ZEN", "", ErrorReputation, true},
		{554, "5.7.1 Rejected: IP found in DNSBL", "", ErrorReputation, true},
		{450, "4.7.1 Client host blacklisted by barracuda", "", ErrorReputation, true},
		// Weak keywords count only on a definitive 5xx AND with an IP-scoped
		// signal — and then degrade only after the Part B threshold.
		{550, "5.7.1 Message blocked due to sender reputation", "", ErrorReputation, false},
		{550, "5.7.1 Our system has detected that this message has been blocked; your IP is the cause", "", ErrorReputation, false},
		// Gmail S3140: a genuine IP-reputation block that also mentions the
		// message content ("unsolicited"). No message-scoped veto, so it must
		// still classify as reputation.
		{550, "5.7.1 Our system has detected an unusual rate of unsolicited mail originating from your IP address. Mail sent from your IP address has been blocked.", "", ErrorReputation, false},
		// Outlook S3150: quotes the exact sending IP literal. No generic IP
		// wording here, so this exercises the source-IP-literal match path.
		{550, "5.7.1 Unfortunately, messages from [192.0.2.1] have been blocked. Please contact your provider.", "192.0.2.1", ErrorReputation, false},
		// Gmail per-message content block: weak keyword but NO IP-scoped
		// signal → NOT reputation, just a permanent (spam) rejection.
		{550, "5.7.1 [2001:db8::1] Our system has detected that this message is likely unsolicited mail. To reduce the amount of spam, this message has been blocked.", "", ErrorPermanent, false},
		// Weak keyword on 5xx with no IP language at all → not reputation.
		{554, "5.7.1 Rejected for policy reasons", "", ErrorPermanent, false},
		// The same weak phrasing on a 4xx is a routine deferral, not a
		// reputation strike.
		{451, "4.7.1 Temporarily blocked, try again later", "", ErrorTemporary, false},
		{450, "4.7.0 Rejected for policy reasons, retry later", "", ErrorTemporary, false},
	}

	for _, tt := range tests {
		err := ClassifyError(tt.code, tt.response, tt.sourceIP, nil)
		if err.Category != tt.expected {
			t.Errorf("Code %d (%q): expected category %s, got %s", tt.code, tt.response, tt.expected, err.Category)
			continue
		}
		if tt.expected == ErrorReputation && err.ImmediateDegrade != tt.immediate {
			t.Errorf("Code %d (%q): expected ImmediateDegrade=%v, got %v", tt.code, tt.response, tt.immediate, err.ImmediateDegrade)
		}
	}
}

func TestClassifyPermanentError_UserNotFound(t *testing.T) {
	responses := []string{
		"550 5.1.1 User not found",
		"550 User unknown",
		"550 5.1.1 <user@example.com>: Recipient address rejected: User unknown in local recipient table",
	}

	for _, response := range responses {
		message := classifyPermanentError(550, response)
		if message != "User not found" {
			t.Errorf("Response '%s': expected 'User not found', got '%s'", response, message)
		}
	}
}

func TestClassifyPermanentError_MailboxUnavailable(t *testing.T) {
	response := "550 Mailbox unavailable"
	message := classifyPermanentError(550, response)

	if message != "Mailbox unavailable" {
		t.Errorf("Expected 'Mailbox unavailable', got '%s'", message)
	}
}

func TestClassifyPermanentError_553(t *testing.T) {
	tests := []struct {
		response string
		expected string
	}{
		{"553 5.1.3 Invalid address syntax", "Invalid mailbox name"},
		{"553 5.7.1 <user@example.com>... Relaying denied", "Relaying denied"},
	}

	for _, tt := range tests {
		message := classifyPermanentError(553, tt.response)
		if message != tt.expected {
			t.Errorf("Response '%s': expected '%s', got '%s'", tt.response, tt.expected, message)
		}
	}
}

func TestClassifyPermanentError_Spam(t *testing.T) {
	responses := []string{
		"550 Message rejected as spam",
		"554 5.7.1 Rejected due to spam content",
	}

	for _, response := range responses {
		err := ClassifyError(550, response, "", nil)
		if !contains(err.Message, "spam") {
			t.Errorf("Response '%s': expected spam-related message, got '%s'", response, err.Message)
		}
	}
}

func TestClassifyTemporaryError_RateLimit(t *testing.T) {
	tests := []struct {
		code     int
		response string
	}{
		{451, "451 Rate limit exceeded"},
		{450, "450 4.7.1 Too many messages from sender"},
	}

	for _, tt := range tests {
		message := classifyTemporaryError(tt.code, tt.response)
		if !contains(message, "rate") {
			t.Errorf("Code %d, Response '%s': expected rate limit message, got '%s'", tt.code, tt.response, message)
		}
	}
}

func TestClassifyTemporaryError_Quota(t *testing.T) {
	response := "452 4.2.2 Mailbox quota exceeded"
	message := classifyTemporaryError(452, response)

	if !contains(message, "quota") && !contains(message, "storage") {
		t.Errorf("Expected quota/storage message, got '%s'", message)
	}
}

func TestShouldDeactivateEmail_UserNotFound(t *testing.T) {
	tests := []struct {
		code     int
		response string
		expected bool
	}{
		{550, "User not found", true},
		{550, "User unknown", true},
		{550, "Recipient not found", true},
		{550, "Mailbox not found", true},
		{550, "Mailbox does not exist", true},
		{553, "Invalid mailbox name", true},
		{553, "5.7.1 Relaying denied", false},
		{550, "Rejected as spam", false},
		{552, "Message too large", false},
		{550, "Relaying denied", false},
	}

	for _, tt := range tests {
		result := ShouldDeactivateEmail(ErrorPermanent, tt.code, tt.response)
		if result != tt.expected {
			t.Errorf("Code %d, response '%s': expected %v, got %v", tt.code, tt.response, tt.expected, result)
		}
	}
}

func TestShouldDeactivateEmail_TemporaryError(t *testing.T) {
	// Temporary errors should never trigger deactivation
	result := ShouldDeactivateEmail(ErrorTemporary, 450, "Mailbox busy")
	if result {
		t.Error("Temporary errors should not trigger deactivation")
	}
}

func TestShouldDeactivateEmail_NetworkError(t *testing.T) {
	// Network errors should never trigger deactivation
	result := ShouldDeactivateEmail(ErrorNetwork, 0, "Connection refused")
	if result {
		t.Error("Network errors should not trigger deactivation")
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		category ErrorCategory
		expected bool
	}{
		{ErrorTemporary, true},
		{ErrorGreylist, true},
		{ErrorNetwork, true},
		{ErrorPermanent, false},
	}

	for _, tt := range tests {
		result := IsRetryable(tt.category)
		if result != tt.expected {
			t.Errorf("Category %s: expected retryable=%v, got %v", tt.category, tt.expected, result)
		}
	}
}

func TestDeliveryError_Error(t *testing.T) {
	// With SMTP code
	err := &DeliveryError{
		Category:     ErrorPermanent,
		SMTPCode:     550,
		SMTPResponse: "User not found",
		Message:      "User not found",
	}

	errorStr := err.Error()
	if !contains(errorStr, "permanent") {
		t.Errorf("Error string should contain category: %s", errorStr)
	}
	if !contains(errorStr, "550") {
		t.Errorf("Error string should contain SMTP code: %s", errorStr)
	}

	// Without SMTP code (network error)
	err2 := &DeliveryError{
		Category: ErrorNetwork,
		Message:  "Connection refused",
	}

	errorStr2 := err2.Error()
	if !contains(errorStr2, "network") {
		t.Errorf("Error string should contain category: %s", errorStr2)
	}
	if !contains(errorStr2, "Connection refused") {
		t.Errorf("Error string should contain message: %s", errorStr2)
	}
}

func TestClassifyNetworkError_DNS(t *testing.T) {
	errors := []error{
		errors.New("lookup example.com: no such host"),
		errors.New("DNS resolution failed"),
	}

	for _, err := range errors {
		result := classifyNetworkError(err)
		if result.Category != ErrorNetwork {
			t.Errorf("Error '%s': expected category %s, got %s", err, ErrorNetwork, result.Category)
		}
		if !contains(result.Message, "DNS") {
			t.Errorf("Error '%s': expected DNS in message, got '%s'", err, result.Message)
		}
	}
}

func TestClassifyNetworkError_Connection(t *testing.T) {
	errors := []error{
		errors.New("connection refused"),
		errors.New("connection reset by peer"),
		errors.New("connection timeout"),
		errors.New("i/o timeout"),
	}

	for _, err := range errors {
		result := classifyNetworkError(err)
		if result.Category != ErrorNetwork {
			t.Errorf("Error '%s': expected category %s, got %s", err, ErrorNetwork, result.Category)
		}
	}
}

func TestClassifyNetworkError_TLS(t *testing.T) {
	errors := []error{
		errors.New("TLS handshake failed"),
		errors.New("x509: certificate has expired"),
	}

	for _, err := range errors {
		result := classifyNetworkError(err)
		if result.Category != ErrorNetwork {
			t.Errorf("Error '%s': expected category %s, got %s", err, ErrorNetwork, result.Category)
		}
		if !contains(result.Message, "TLS") {
			t.Errorf("Error '%s': expected TLS in message, got '%s'", err, result.Message)
		}
	}
}

// Helper function to check if string contains substring (case-insensitive)
func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
