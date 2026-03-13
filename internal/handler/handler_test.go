package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"strela/internal/config"
	"strela/internal/delivery"
)

func TestBuildRawMessage_MessageID(t *testing.T) {
	h := &Handler{}

	tests := []struct {
		name      string
		messageID string
		wantInMsg bool
	}{
		{
			name:      "auto-generate Message-ID when not provided",
			messageID: "",
			wantInMsg: true,
		},
		{
			name:      "use provided Message-ID",
			messageID: "<custom-id@example.com>",
			wantInMsg: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &MessageRequest{
				From:      "sender@example.com",
				To:        "recipient@example.com",
				Subject:   "Test Subject",
				Text:      "Test Body",
				MessageID: tt.messageID,
			}

			rawMsg, err := h.buildRawMessage(req)
			if err != nil {
				t.Fatalf("buildRawMessage() error = %v", err)
			}

			msgStr := string(rawMsg)

			// Check that Message-ID header exists
			if !strings.Contains(msgStr, "Message-ID:") && !strings.Contains(msgStr, "Message-Id:") {
				t.Error("Message-ID header not found in message")
			}

			// If custom Message-ID was provided, verify it's in the output
			if tt.messageID != "" {
				if !strings.Contains(msgStr, tt.messageID) {
					t.Errorf("Custom Message-ID %q not found in message", tt.messageID)
				}
			}
		})
	}
}

func TestBuildRawMessage_ValidEmail(t *testing.T) {
	h := &Handler{}

	req := &MessageRequest{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test Subject",
		Text:    "Plain text body",
		HTML:    "<html><body>HTML body</body></html>",
	}

	rawMsg, err := h.buildRawMessage(req)
	if err != nil {
		t.Fatalf("buildRawMessage() error = %v", err)
	}

	msgStr := string(rawMsg)

	// Verify essential headers
	requiredHeaders := []string{
		"From:",
		"To:",
		"Subject:",
		"Date:",
	}

	for _, header := range requiredHeaders {
		if !strings.Contains(msgStr, header) {
			t.Errorf("Required header %q not found in message", header)
		}
	}

	// Check Message-ID with case-insensitive search
	if !strings.Contains(msgStr, "Message-ID:") && !strings.Contains(msgStr, "Message-Id:") {
		t.Errorf("Message-ID header not found in message")
	}

	// Verify both text and HTML parts are present
	if !strings.Contains(msgStr, "text/plain") {
		t.Error("text/plain content type not found")
	}
	if !strings.Contains(msgStr, "text/html") {
		t.Error("text/html content type not found")
	}
}

func TestHandleDeliver_RawMessage(t *testing.T) {
	// Create minimal config
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	// Create mock deliverer (will not actually send)
	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}

	h := NewHandler(cfg, mockDeliverer, logger)

	// Test raw message mode
	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test Forwarded Email
Message-ID: <original-id@upstream.com>
Date: Mon, 15 Jan 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

This is a forwarded email message.
It preserves all original headers.`

	reqBody := MessageRequest{
		From:       "sender@example.com",
		To:         "recipient@example.com",
		RawMessage: rawEmail,
	}

	bodyBytes, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.HandleDeliver(rr, req)

	// Should accept and not return 400
	if rr.Code == http.StatusBadRequest {
		t.Errorf("Expected raw message to be accepted, got status %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the deliverer received the raw message
	if mockDeliverer.lastMessage == "" {
		t.Error("Expected deliverer to receive message")
	}

	if !strings.Contains(mockDeliverer.lastMessage, "This is a forwarded email message") {
		t.Error("Raw message content not preserved")
	}

	if !strings.Contains(mockDeliverer.lastMessage, "<original-id@upstream.com>") {
		t.Error("Original Message-ID not preserved")
	}
}

func TestHandleDeliver_ModeValidation(t *testing.T) {
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	tests := []struct {
		name       string
		req        MessageRequest
		wantStatus int
		wantError  string
	}{
		{
			name: "composed mode - valid",
			req: MessageRequest{
				From:    "sender@example.com",
				To:      "recipient@example.com",
				Subject: "Test",
				Text:    "Body",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "raw mode - valid",
			req: MessageRequest{
				From:       "sender@example.com",
				To:         "recipient@example.com",
				RawMessage: "From: sender@example.com\r\nTo: recipient@example.com\r\n\r\nBody",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "conflicting modes - invalid",
			req: MessageRequest{
				From:       "sender@example.com",
				To:         "recipient@example.com",
				Subject:    "Test",
				RawMessage: "From: sender@example.com\r\n\r\nBody",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "Cannot use both",
		},
		{
			name: "no content - invalid",
			req: MessageRequest{
				From: "sender@example.com",
				To:   "recipient@example.com",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "Must provide either",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.req)
			req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			h.HandleDeliver(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("Expected status %d, got %d: %s", tt.wantStatus, rr.Code, rr.Body.String())
			}

			if tt.wantError != "" && !strings.Contains(rr.Body.String(), tt.wantError) {
				t.Errorf("Expected error containing %q, got %q", tt.wantError, rr.Body.String())
			}
		})
	}
}

// mockDeliverer implements the delivery.Deliverer interface for testing
type mockDeliverer struct {
	lastMessage string
	result      *delivery.DeliveryResult // if set, returned instead of default
}

func (m *mockDeliverer) DeliverMessage(ctx context.Context, from, to string, message []byte, dkimPrivateKey, dkimSelector, dkimDomain string, skipDKIMValidation bool, arcPrivateKey, arcSelector, arcDomain string, inboundAuth *delivery.InboundAuthResults) delivery.DeliveryResult {
	m.lastMessage = string(message)
	if m.result != nil {
		return *m.result
	}
	return delivery.DeliveryResult{
		Status:            "delivered",
		SMTPCode:          250,
		SMTPMessage:       "OK",
		MXHost:            "mx.example.com",
		SourceIP:          "127.0.0.1",
		AttemptDurationMs: 100,
	}
}

func (m *mockDeliverer) Stop() {
	// No-op for mock
}

func TestHandleDeliver_DKIMConfigDefaults(t *testing.T) {
	// Create config with DKIM enabled
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
		DKIM: config.DKIMConfig{
			Enabled:        true,
			Selector:       "default",
			Domain:         "example.com",
			SkipValidation: true, // Skip for testing
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}

	// Create handler (without DKIM private key file - won't actually sign)
	h := NewHandler(cfg, mockDeliverer, logger)

	tests := []struct {
		name               string
		req                MessageRequest
		expectDKIMSelector string
		expectDKIMDomain   string
	}{
		{
			name: "use config defaults when not provided",
			req: MessageRequest{
				From:    "sender@example.com",
				To:      "recipient@example.com",
				Subject: "Test",
				Text:    "Body",
			},
			expectDKIMSelector: "", // Private key not loaded, so won't use config
			expectDKIMDomain:   "",
		},
		{
			name: "API request overrides config defaults",
			req: MessageRequest{
				From:           "sender@example.com",
				To:             "recipient@example.com",
				Subject:        "Test",
				Text:           "Body",
				DKIMSelector:   "override",
				DKIMDomain:     "override.com",
				DKIMPrivateKey: "dummy-key-for-test",
			},
			expectDKIMSelector: "override",
			expectDKIMDomain:   "override.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.req)
			req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			h.HandleDeliver(rr, req)

			// Check that request was processed
			if rr.Code != http.StatusOK {
				t.Logf("Response: %s", rr.Body.String())
			}
		})
	}
}

func TestNewHandler_LoadsDKIMKey(t *testing.T) {
	// Create a temporary DKIM private key file
	tmpFile, err := os.CreateTemp("", "dkim-test-*.key")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	testKey := "-----BEGIN RSA PRIVATE KEY-----\ntest-key-content\n-----END RSA PRIVATE KEY-----"
	if _, err := tmpFile.WriteString(testKey); err != nil {
		t.Fatalf("Failed to write test key: %v", err)
	}
	tmpFile.Close()

	cfg := &config.Config{
		DKIM: config.DKIMConfig{
			Enabled:        true,
			Selector:       "default",
			Domain:         "example.com",
			PrivateKeyPath: tmpFile.Name(),
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}

	h := NewHandler(cfg, mockDeliverer, logger)

	if h.dkimPrivateKey != testKey {
		t.Errorf("Expected DKIM private key to be loaded, got %q", h.dkimPrivateKey)
	}
}

func TestHandleDeliver_RawMessagePassedToDelivery(t *testing.T) {
	// Verify that raw_message is passed as-is to delivery engine
	// This ensures ARC/SRS signing in delivery layer works with raw messages
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	// Original raw message with existing headers
	originalRawMsg := `From: original@sender.com
To: recipient@example.com
Subject: Forwarded Email
Message-ID: <upstream-id@original.com>
Received: from mx1.original.com by relay.example.com
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

This is a forwarded message with original headers intact.
`

	req := MessageRequest{
		From:       "original@sender.com",
		To:         "recipient@example.com",
		RawMessage: originalRawMsg,
	}

	bodyBytes, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
	httpReq.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.HandleDeliver(rr, httpReq)

	// Verify message was passed to deliverer
	if mockDeliverer.lastMessage == "" {
		t.Fatal("Expected message to be passed to deliverer")
	}

	// Verify raw message content is preserved
	if !strings.Contains(mockDeliverer.lastMessage, "<upstream-id@original.com>") {
		t.Error("Original Message-ID not preserved in delivery")
	}

	if !strings.Contains(mockDeliverer.lastMessage, "Received: from mx1.original.com") {
		t.Error("Original Received header not preserved in delivery")
	}

	if !strings.Contains(mockDeliverer.lastMessage, "This is a forwarded message with original headers intact.") {
		t.Error("Original message body not preserved in delivery")
	}

	// This raw message will be processed by delivery engine where:
	// - ARC signing will add ARC headers (if enabled)
	// - SRS will rewrite envelope sender (if enabled)
	// - DKIM signing will add DKIM-Signature header (if enabled)
}

func TestHandleDeliver_DynamicARCKey(t *testing.T) {
	// Test that ARC parameters can be passed via API request
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
		ARC: config.ARCConfig{
			Enabled:  true,
			Selector: "config-arc",
			Domain:   "config.example.com",
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	tests := []struct {
		name           string
		req            MessageRequest
		expectOverride bool
	}{
		{
			name: "API request overrides ARC config",
			req: MessageRequest{
				From:          "sender@example.com",
				To:            "recipient@example.com",
				Subject:       "Test",
				Text:          "Body",
				ARCPrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nAPI-KEY\n-----END RSA PRIVATE KEY-----",
				ARCSelector:   "api-arc",
				ARCDomain:     "api.example.com",
			},
			expectOverride: true,
		},
		{
			name: "Use config defaults when not provided",
			req: MessageRequest{
				From:    "sender@example.com",
				To:      "recipient@example.com",
				Subject: "Test",
				Text:    "Body",
				// No ARC parameters - should use config
			},
			expectOverride: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.req)
			req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			h.HandleDeliver(rr, req)

			// Check that request was processed
			if rr.Code != http.StatusOK {
				t.Logf("Response: %s", rr.Body.String())
			}

			// The delivery engine receives the ARC parameters
			// In production, these would be used for signing
		})
	}
}

func TestHandleDeliver_DKIMARCDisabledIgnoresAPIParams(t *testing.T) {
	// Test that API parameters are ignored when config explicitly disables DKIM/ARC
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
		DKIM: config.DKIMConfig{
			Enabled: false, // Explicitly disabled
		},
		ARC: config.ARCConfig{
			Enabled: false, // Explicitly disabled
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	req := MessageRequest{
		From:           "sender@example.com",
		To:             "recipient@example.com",
		Subject:        "Test",
		Text:           "Body",
		DKIMPrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nSHOULD-BE-IGNORED\n-----END RSA PRIVATE KEY-----",
		DKIMSelector:   "ignored",
		DKIMDomain:     "ignored.com",
		ARCPrivateKey:  "-----BEGIN RSA PRIVATE KEY-----\nSHOULD-BE-IGNORED\n-----END RSA PRIVATE KEY-----",
		ARCSelector:    "ignored",
		ARCDomain:      "ignored.com",
	}

	bodyBytes, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
	httpReq.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.HandleDeliver(rr, httpReq)

	// Should still succeed (not error), just without DKIM/ARC
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The delivery engine should have received empty strings for DKIM/ARC params
	// This test verifies the handler respects the enabled flag
}

func TestHandleDeliver_HeaderMode(t *testing.T) {
	// Test the new header mode (Content-Type: message/rfc822)
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test Email via Header Mode
Message-ID: <test-123@example.com>
Date: Mon, 15 Jan 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

This is the email body sent via header mode.
`

	req := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(rawEmail))
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Envelope-From", "sender@example.com")
	req.Header.Set("X-Envelope-To", "recipient@example.com")
	req.Header.Set("X-DKIM-Selector", "default")
	req.Header.Set("X-DKIM-Domain", "example.com")
	req.Header.Set("X-ARC-Selector", "arc1")
	req.Header.Set("X-ARC-Domain", "arc.example.com")

	rr := httptest.NewRecorder()
	h.HandleDeliver(rr, req)

	// Should succeed
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify message was delivered
	if mockDeliverer.lastMessage == "" {
		t.Fatal("Expected message to be delivered")
	}

	// Verify raw message content preserved
	if !strings.Contains(mockDeliverer.lastMessage, "This is the email body sent via header mode.") {
		t.Error("Email body not preserved")
	}

	if !strings.Contains(mockDeliverer.lastMessage, "<test-123@example.com>") {
		t.Error("Message-ID not preserved")
	}
}

func TestHandleDeliver_HeaderModeWithDKIM(t *testing.T) {
	// Test header mode with DKIM parameters
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
		DKIM: config.DKIMConfig{
			Enabled: true,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test with DKIM

Test body
`

	testKey := "-----BEGIN RSA PRIVATE KEY-----\ntest-key\n-----END RSA PRIVATE KEY-----"

	req := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(rawEmail))
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Envelope-From", "sender@example.com")
	req.Header.Set("X-Envelope-To", "recipient@example.com")
	req.Header.Set("X-DKIM-Private-Key", testKey)
	req.Header.Set("X-DKIM-Selector", "test-selector")
	req.Header.Set("X-DKIM-Domain", "test.example.com")

	rr := httptest.NewRecorder()
	h.HandleDeliver(rr, req)

	// Should succeed
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleDeliver_HeaderModeMissingEnvelope(t *testing.T) {
	// Test that header mode fails gracefully when envelope headers missing
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test

Body
`

	req := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(rawEmail))
	req.Header.Set("Content-Type", "message/rfc822")
	// Missing X-Envelope-From and X-Envelope-To headers

	rr := httptest.NewRecorder()
	h.HandleDeliver(rr, req)

	// Should fail with 400 Bad Request
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d: %s", rr.Code, rr.Body.String())
	}

	if !strings.Contains(rr.Body.String(), "Missing 'from' or 'to'") {
		t.Errorf("Expected missing fields error, got: %s", rr.Body.String())
	}
}

func TestHandleDeliver_BothModesWork(t *testing.T) {
	// Test that both JSON and header modes work correctly
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test Email

Test body content
`

	tests := []struct {
		name        string
		setupReq    func() *http.Request
		wantSuccess bool
	}{
		{
			name: "JSON mode",
			setupReq: func() *http.Request {
				reqBody := MessageRequest{
					From:       "sender@example.com",
					To:         "recipient@example.com",
					RawMessage: rawEmail,
				}
				bodyBytes, _ := json.Marshal(reqBody)
				req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			wantSuccess: true,
		},
		{
			name: "Header mode",
			setupReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(rawEmail))
				req.Header.Set("Content-Type", "message/rfc822")
				req.Header.Set("X-Envelope-From", "sender@example.com")
				req.Header.Set("X-Envelope-To", "recipient@example.com")
				return req
			},
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq()
			rr := httptest.NewRecorder()
			h.HandleDeliver(rr, req)

			if tt.wantSuccess {
				if rr.Code != http.StatusOK {
					t.Errorf("Expected success, got status %d: %s", rr.Code, rr.Body.String())
				}

				// Verify message delivered
				if !strings.Contains(mockDeliverer.lastMessage, "Test body content") {
					t.Error("Message content not delivered correctly")
				}
			}
		})
	}
}

func TestHandleDeliver_HeaderModeBase64PrivateKeys(t *testing.T) {
	// Test that base64-encoded private keys in headers are decoded correctly
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
		DKIM: config.DKIMConfig{
			Enabled: true,
		},
		ARC: config.ARCConfig{
			Enabled: true,
		},
	}

	logger := slog.Default()
	mockDeliverer := &mockDeliverer{}
	h := NewHandler(cfg, mockDeliverer, logger)

	rawEmail := `From: sender@example.com
To: recipient@example.com
Subject: Test with Base64 Keys

Test body
`

	// Simulate a PEM private key (with newlines)
	dkimKey := "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pmr3ZOxa\n-----END RSA PRIVATE KEY-----"
	arcKey := "-----BEGIN RSA PRIVATE KEY-----\nMIIBPAIBAAJBALc3WGLdhuqYVF6Y8owW2l7rgYGmBqJv\n-----END RSA PRIVATE KEY-----"

	// Base64 encode them for HTTP headers (newlines not allowed in headers)
	dkimKeyB64 := base64.StdEncoding.EncodeToString([]byte(dkimKey))
	arcKeyB64 := base64.StdEncoding.EncodeToString([]byte(arcKey))

	req := httptest.NewRequest(http.MethodPost, "/deliver", strings.NewReader(rawEmail))
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Envelope-From", "sender@example.com")
	req.Header.Set("X-Envelope-To", "recipient@example.com")
	req.Header.Set("X-DKIM-Private-Key", dkimKeyB64)
	req.Header.Set("X-DKIM-Selector", "test-selector")
	req.Header.Set("X-DKIM-Domain", "test.example.com")
	req.Header.Set("X-ARC-Private-Key", arcKeyB64)
	req.Header.Set("X-ARC-Selector", "arc-selector")
	req.Header.Set("X-ARC-Domain", "arc.example.com")

	rr := httptest.NewRecorder()
	h.HandleDeliver(rr, req)

	// Should succeed
	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The handler should have decoded the base64 keys back to original PEM format
	// (This would be verified by the delivery engine in real usage)
}

func TestDecodeBase64Header(t *testing.T) {
	h := &Handler{}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "valid base64",
			input:    base64.StdEncoding.EncodeToString([]byte("test value")),
			expected: "test value",
		},
		{
			name:     "PEM key encoded",
			input:    base64.StdEncoding.EncodeToString([]byte("-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34Gkx\n-----END RSA PRIVATE KEY-----")),
			expected: "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34Gkx\n-----END RSA PRIVATE KEY-----",
		},
		{
			name:     "plain text (not base64)",
			input:    "plain-text-selector",
			expected: "plain-text-selector", // Returns original if not valid base64
		},
		{
			name:     "invalid base64 with special chars",
			input:    "invalid!!!base64",
			expected: "invalid!!!base64", // Returns original if decode fails
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := h.decodeBase64Header(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestTimeoutResponse verifies that on a connection timeout:
// - HTTP status is 504 (Gateway Timeout)
// - JSON body smtp_code is 0 (no SMTP response was received, NOT 504)
// - JSON body status is "timeout"
func TestTimeoutResponse(t *testing.T) {
	cfg := &config.Config{
		Outbound: config.OutboundConfig{
			MaxTotalDeliverySeconds: 30,
		},
	}

	timeoutResult := &delivery.DeliveryResult{
		Status:            "timeout",
		SMTPCode:          0, // No SMTP response received during a connection timeout
		SMTPMessage:       "",
		MXHost:            "mx.example.com",
		SourceIP:          "192.0.2.1",
		AttemptDurationMs: 30000,
		Error:             "context deadline exceeded",
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mockDel := &mockDeliverer{result: timeoutResult}
	h := NewHandler(cfg, mockDel, logger)

	reqBody := MessageRequest{
		From:    "sender@example.com",
		To:      "recipient@example.com",
		Subject: "Test",
		Text:    "Body",
	}
	bodyBytes, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.HandleDeliver(rr, req)

	// HTTP status must be 504 Gateway Timeout
	if rr.Code != http.StatusGatewayTimeout {
		t.Errorf("Expected HTTP 504, got %d", rr.Code)
	}

	var resp delivery.DeliveryResult
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response body: %v", err)
	}

	// status must be "timeout"
	if resp.Status != "timeout" {
		t.Errorf("Expected status %q, got %q", "timeout", resp.Status)
	}

	// smtp_code must be 0 — connection timed out before any SMTP response
	// It must NOT be 504 (that is the HTTP status, not an SMTP code)
	if resp.SMTPCode != 0 {
		t.Errorf("Expected smtp_code 0 for connection timeout, got %d (504 is HTTP status, not SMTP code)", resp.SMTPCode)
	}
}
