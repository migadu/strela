package delivery

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyError_GreylistCode(t *testing.T) {
	err := ClassifyError(421, "4.7.1 Greylisted, please try again later", nil)

	if err.Category != ErrorGreylist {
		t.Errorf("Expected category %s, got %s", ErrorGreylist, err.Category)
	}

	if err.SMTPCode != 421 {
		t.Errorf("Expected SMTP code 421, got %d", err.SMTPCode)
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
		{454, "TLS failed", "TLS negotiation failed"},
	}

	for _, tt := range tests {
		err := ClassifyError(tt.code, tt.response, nil)

		if err.Category != ErrorTemporary {
			t.Errorf("Code %d: expected category %s, got %s", tt.code, ErrorTemporary, err.Category)
		}

		if err.SMTPCode != tt.code {
			t.Errorf("Code %d: expected SMTP code %d, got %d", tt.code, tt.code, err.SMTPCode)
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
		err := ClassifyError(tt.code, tt.response, nil)

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
		err := ClassifyError(0, "", tt.err)

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
	err := ClassifyError(250, "OK", nil)
	if err != nil {
		t.Errorf("Expected nil for success code 250, got %v", err)
	}

	err = ClassifyError(220, "Service ready", nil)
	if err != nil {
		t.Errorf("Expected nil for success code 220, got %v", err)
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

func TestClassifyPermanentError_Spam(t *testing.T) {
	responses := []string{
		"550 Message rejected as spam",
		"554 5.7.1 Rejected due to spam content",
	}

	for _, response := range responses {
		err := ClassifyError(550, response, nil)
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
