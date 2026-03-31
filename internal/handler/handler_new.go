package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/quotedprintable"
	"net/http"
	"os"
	"strings"
	"time"

	"strela/internal/config"
	"strela/internal/delivery"

	"github.com/emersion/go-message/mail"
)

// DeliveryEngine defines the interface for message delivery
type DeliveryEngine interface {
	DeliverMessage(ctx context.Context, from, to string, message []byte, dkimPrivateKey, dkimSelector, dkimDomain string, skipDKIMValidation bool, arcPrivateKey, arcSelector, arcDomain string, inboundAuth *delivery.InboundAuthResults) delivery.DeliveryResult
	Stop()
}

// Handler manages HTTP API endpoints.
type Handler struct {
	config         *config.Config
	deliveryEngine DeliveryEngine
	logger         *slog.Logger
	dkimPrivateKey string // Loaded from config if dkim.enabled=true
}

// NewHandler creates a new HTTP handler.
func NewHandler(cfg *config.Config, engine DeliveryEngine, logger *slog.Logger) *Handler {
	h := &Handler{
		config:         cfg,
		deliveryEngine: engine,
		logger:         logger,
	}

	// Load DKIM private key if enabled
	if cfg.DKIM.Enabled && cfg.DKIM.PrivateKeyPath != "" {
		keyData, err := os.ReadFile(cfg.DKIM.PrivateKeyPath)
		if err != nil {
			logger.Error("failed to read DKIM private key", "error", err, "path", cfg.DKIM.PrivateKeyPath)
		} else {
			h.dkimPrivateKey = string(keyData)
			logger.Info("DKIM signing enabled", "selector", cfg.DKIM.Selector, "domain", cfg.DKIM.Domain)
		}
	}

	return h
}

// Attachment represents an email attachment
type Attachment struct {
	Filename    string `json:"filename"`     // Filename for Content-Disposition
	ContentType string `json:"content_type"` // MIME type (e.g., "application/pdf")
	Content     string `json:"content"`      // Base64-encoded content
}

// MessageRequest represents the JSON request body for message submission.
// Supports two modes:
// 1. Composed mode: Provide from, to, subject, text/html (Strela builds MIME message)
// 2. Raw mode: Provide from, to, raw_message (Strela forwards pre-built RFC822 message)
type MessageRequest struct {
	From               string                       `json:"from"`
	To                 string                       `json:"to"`
	Subject            string                       `json:"subject,omitempty"`              // Required for composed mode, ignored for raw mode
	Text               string                       `json:"text,omitempty"`                 // Optional for composed mode, ignored for raw mode
	HTML               string                       `json:"html,omitempty"`                 // Optional for composed mode, ignored for raw mode
	Attachments        []Attachment                 `json:"attachments,omitempty"`          // Optional attachments for composed mode
	Headers            map[string]string            `json:"headers,omitempty"`              // Optional custom headers (e.g., {"Reply-To": "support@example.com"})
	MessageID          string                       `json:"message_id,omitempty"`           // Optional Message-ID for composed mode; auto-generated if not provided
	RawMessage         string                       `json:"raw_message,omitempty"`          // Raw RFC822 message (forwarding mode) - mutually exclusive with subject/text/html
	DKIMPrivateKey     string                       `json:"dkim_private_key,omitempty"`     // Override config DKIM key
	DKIMSelector       string                       `json:"dkim_selector,omitempty"`        // Override config DKIM selector
	DKIMDomain         string                       `json:"dkim_domain,omitempty"`          // Override config DKIM domain
	SkipDKIMValidation bool                         `json:"skip_dkim_validation,omitempty"` // Skip DNS validation (faster but less safe)
	ARCPrivateKey      string                       `json:"arc_private_key,omitempty"`      // Override config ARC key (for dynamic/multi-tenant scenarios)
	ARCSelector        string                       `json:"arc_selector,omitempty"`         // Override config ARC selector
	ARCDomain          string                       `json:"arc_domain,omitempty"`           // Override config ARC domain
	InboundAuth        *delivery.InboundAuthResults `json:"inbound_auth,omitempty"`         // Auth results from the previous hop (forwarding only); nil for normal outbound
}

// generateTraceID generates a random 16-character hex trace ID for a delivery session.
func generateTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: use timestamp-based ID (unlikely to fail, but safe)
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// HandleDeliver handles synchronous message delivery requests.
// Supports two modes:
// 1. JSON mode: Content-Type: application/json with MessageRequest body
// 2. Header mode: Content-Type: message/rfc822 with raw RFC822 body and HTTP headers
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	// Generate a unique trace ID for this delivery session.
	traceID := generateTraceID()

	h.logger.Debug("received delivery request",
		"trace_id", traceID,
		"remote_addr", r.RemoteAddr,
		"method", r.Method,
		"url", r.URL.String())

	if r.Method != http.MethodPost {
		h.logger.Debug("method not allowed", "method", r.Method, "trace_id", traceID)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authentication
	if h.config.Inbound.AuthToken != "" {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.config.Inbound.AuthToken)) != 1 {
			h.logger.Warn("authentication failed", "remote_addr", r.RemoteAddr, "trace_id", traceID)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.logger.Debug("authentication successful", "remote_addr", r.RemoteAddr, "trace_id", traceID)
	}

	// 2. Enforce request body size limit
	maxBody := h.config.Inbound.MaxBodySizeBytes
	if maxBody <= 0 {
		maxBody = 35 * 1024 * 1024 // 35MB default
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	// 3. Determine mode based on Content-Type
	contentType := r.Header.Get("Content-Type")
	isHeaderMode := strings.HasPrefix(contentType, "message/rfc822")

	var req MessageRequest
	if isHeaderMode {
		// Header mode: parse from HTTP headers + raw body
		h.logger.Debug("using header mode (Content-Type: message/rfc822)")
		req = h.parseHeaderMode(r)
	} else {
		// JSON mode (legacy): parse from JSON body
		h.logger.Debug("using JSON mode")
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.logger.Debug("failed to decode JSON payload", "error", err, "remote_addr", r.RemoteAddr)
			http.Error(w, "Invalid JSON or request body too large", http.StatusBadRequest)
			return
		}
	}
	h.logger.Debug("parsed message request",
		"from", req.From,
		"to", req.To,
		"subject", req.Subject,
		"has_raw_message", req.RawMessage != "",
		"has_dkim_key", req.DKIMPrivateKey != "",
		"dkim_selector", req.DKIMSelector,
		"dkim_domain", req.DKIMDomain,
		"has_arc_key", req.ARCPrivateKey != "",
		"arc_selector", req.ARCSelector,
		"arc_domain", req.ARCDomain)

	// 4. Validation
	if req.From == "" || req.To == "" {
		h.logger.Debug("missing required fields", "from", req.From, "to", req.To)
		http.Error(w, "Missing 'from' or 'to' fields", http.StatusBadRequest)
		return
	}

	// Validate email address format before passing to SMTP layer.
	// From allows null sender (<>) for bounce/DSN messages.
	if err := validateEmailAddress(req.From, "from", true); err != nil {
		h.logger.Debug("invalid 'from' address", "from", req.From, "error", err, "trace_id", traceID)
		http.Error(w, fmt.Sprintf("Invalid 'from' address: %v", err), http.StatusBadRequest)
		return
	}
	if err := validateEmailAddress(req.To, "to", false); err != nil {
		h.logger.Debug("invalid 'to' address", "to", req.To, "error", err, "trace_id", traceID)
		http.Error(w, fmt.Sprintf("Invalid 'to' address: %v", err), http.StatusBadRequest)
		return
	}

	// Log incoming delivery request at INFO level
	h.logger.Info("delivery request received",
		"trace_id", traceID,
		"from", req.From,
		"to", req.To,
		"remote_addr", r.RemoteAddr)

	// Determine mode: raw message or composed
	isRawMode := req.RawMessage != ""
	isComposedMode := req.Subject != "" || req.Text != "" || req.HTML != ""

	if isRawMode && isComposedMode {
		h.logger.Warn("conflicting fields: raw_message and composed fields both provided")
		http.Error(w, "Cannot use both 'raw_message' and composed fields (subject/text/html)", http.StatusBadRequest)
		return
	}

	if !isRawMode && !isComposedMode {
		h.logger.Warn("missing message content")
		http.Error(w, "Must provide either 'raw_message' or at least one of 'subject'/'text'/'html'", http.StatusBadRequest)
		return
	}

	// 5. Prepare message payload
	var rawMessage []byte
	var err error

	if isRawMode {
		h.logger.Debug("using raw message mode", "from", req.From, "to", req.To, "size", len(req.RawMessage))
		rawMessage = []byte(req.RawMessage)
	} else {
		h.logger.Debug("building MIME message", "from", req.From, "to", req.To)
		rawMessage, err = h.buildRawMessage(&req)
		if err != nil {
			h.logger.Error("failed to build MIME message", "error", err)
			http.Error(w, "Failed to build message", http.StatusInternalServerError)
			return
		}
		h.logger.Debug("MIME message built", "size", len(rawMessage))
	}

	// 6. Apply DKIM config (respects enabled flag)
	var dkimPrivateKey, dkimSelector, dkimDomain string
	var skipDKIMValidation bool

	// Only apply DKIM if config enabled=true OR API provides parameters
	if h.config.DKIM.Enabled {
		// Config enabled: use config defaults + allow API override
		dkimPrivateKey = req.DKIMPrivateKey
		dkimSelector = req.DKIMSelector
		dkimDomain = req.DKIMDomain
		skipDKIMValidation = req.SkipDKIMValidation

		// Apply config defaults if not provided in request
		if dkimPrivateKey == "" && h.dkimPrivateKey != "" {
			dkimPrivateKey = h.dkimPrivateKey
			h.logger.Debug("using DKIM private key from config")
		}
		if dkimSelector == "" && h.config.DKIM.Selector != "" {
			dkimSelector = h.config.DKIM.Selector
			h.logger.Debug("using DKIM selector from config", "selector", dkimSelector)
		}
		if dkimDomain == "" && h.config.DKIM.Domain != "" {
			dkimDomain = h.config.DKIM.Domain
			h.logger.Debug("using DKIM domain from config", "domain", dkimDomain)
		}
		if !req.SkipDKIMValidation && h.config.DKIM.SkipValidation {
			skipDKIMValidation = h.config.DKIM.SkipValidation
			h.logger.Debug("using DKIM skip_validation from config", "skip", skipDKIMValidation)
		}
	} else {
		// Config disabled: ignore all DKIM parameters from API
		if req.DKIMPrivateKey != "" || req.DKIMSelector != "" || req.DKIMDomain != "" {
			h.logger.Warn("DKIM parameters provided but DKIM is disabled in config - ignoring")
		}
		// Leave all DKIM params as empty strings
	}

	// Apply ARC config (respects enabled flag)
	var arcPrivateKey, arcSelector, arcDomain string

	// Only apply ARC if config enabled=true OR API provides parameters
	if h.config.ARC.Enabled {
		// Config enabled: use config defaults + allow API override
		arcPrivateKey = req.ARCPrivateKey
		arcSelector = req.ARCSelector
		arcDomain = req.ARCDomain

		h.logger.Debug("ARC parameters from API",
			"has_private_key", req.ARCPrivateKey != "",
			"selector", arcSelector,
			"domain", arcDomain)

		// Apply config defaults if not provided in request
		if arcSelector == "" && h.config.ARC.Selector != "" {
			arcSelector = h.config.ARC.Selector
			h.logger.Debug("using ARC selector from config", "selector", arcSelector)
		}
		if arcDomain == "" && h.config.ARC.Domain != "" {
			arcDomain = h.config.ARC.Domain
			h.logger.Debug("using ARC domain from config", "domain", arcDomain)
		}
	} else {
		// Config disabled: ignore all ARC parameters from API
		if req.ARCPrivateKey != "" || req.ARCSelector != "" || req.ARCDomain != "" {
			h.logger.Warn("ARC parameters provided but ARC is disabled in config - ignoring")
		}
		// Leave all ARC params as empty strings
	}

	// 7. Create context with timeout and trace ID
	timeout := time.Duration(h.config.Outbound.MaxTotalDeliverySeconds) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	ctx = delivery.WithTraceID(ctx, traceID)

	// 8. Synchronous Delivery
	h.logger.Debug("starting synchronous delivery",
		"trace_id", traceID,
		"from", req.From,
		"to", req.To,
		"has_arc_key", arcPrivateKey != "",
		"arc_selector", arcSelector,
		"arc_domain", arcDomain)
	result := h.deliveryEngine.DeliverMessage(ctx, req.From, req.To, rawMessage, dkimPrivateKey, dkimSelector, dkimDomain, skipDKIMValidation, arcPrivateKey, arcSelector, arcDomain, req.InboundAuth)
	h.logger.Debug("delivery attempt finished",
		"trace_id", traceID,
		"status", result.Status,
		"mx", result.MXHost,
		"source_ip", result.SourceIP,
		"duration_ms", result.AttemptDurationMs)

	// 9. Map result to HTTP status
	statusCode := mapDeliveryStatusToHTTP(result.Status)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		h.logger.Error("failed to encode response", "error", err)
	}
}

func mapDeliveryStatusToHTTP(status string) int {
	switch status {
	case "delivered":
		return http.StatusOK // 200 - success
	case "temp_fail":
		return 429 // 429 Too Many Requests - temporary failure, retry with backoff (SMTP 4xx)
	case "rate_limit":
		return http.StatusTooManyRequests // 429 - retry later (Fail Fast)
	case "hard_bounce":
		return 554 // 554 Transaction Failed - permanent failure, do not retry (SMTP 5xx)
	case "timeout":
		return http.StatusGatewayTimeout // 504 - delivery timeout exceeded
	case "error":
		return http.StatusInternalServerError // 500 - internal error
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handler) buildRawMessage(req *MessageRequest) ([]byte, error) {
	var buf bytes.Buffer
	var header mail.Header
	header.SetDate(time.Now())
	header.SetAddressList("From", []*mail.Address{{Address: req.From}})
	header.SetAddressList("To", []*mail.Address{{Address: req.To}})
	header.SetSubject(req.Subject)

	// Set Message-ID: use provided value or auto-generate
	if req.MessageID != "" {
		header.SetMessageID(req.MessageID)
	} else {
		if err := header.GenerateMessageID(); err != nil {
			return nil, fmt.Errorf("failed to generate Message-ID: %w", err)
		}
	}

	// Apply custom headers (reject any with CRLF sequences to prevent header injection)
	for key, value := range req.Headers {
		if strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid custom header %q: contains prohibited CRLF characters", key)
		}
		header.Set(key, value)
	}

	// Determine if we need multipart structure
	hasAttachments := len(req.Attachments) > 0
	hasMultipleBodyParts := req.HTML != "" && req.Text != ""
	needsMultipart := hasAttachments || hasMultipleBodyParts

	// For multipart messages, use CreateWriter
	if needsMultipart {
		mw, err := mail.CreateWriter(&buf, header)
		if err != nil {
			return nil, err
		}

		// Handle body parts
		if hasMultipleBodyParts {
			// multipart/alternative for text + html
			iw, err := mw.CreateInline()
			if err != nil {
				return nil, err
			}
			if err := createPart(iw, "text/plain", req.Text); err != nil {
				return nil, fmt.Errorf("failed to create text part: %w", err)
			}
			if err := createPart(iw, "text/html", req.HTML); err != nil {
				return nil, fmt.Errorf("failed to create HTML part: %w", err)
			}
			iw.Close()
		} else if req.HTML != "" {
			if err := createPart(mw, "text/html", req.HTML); err != nil {
				return nil, fmt.Errorf("failed to create HTML part: %w", err)
			}
		} else if req.Text != "" {
			if err := createPart(mw, "text/plain", req.Text); err != nil {
				return nil, fmt.Errorf("failed to create text part: %w", err)
			}
		}

		// Handle attachments
		for _, att := range req.Attachments {
			if err := createAttachment(mw, att); err != nil {
				mw.Close()
				return nil, fmt.Errorf("failed to create attachment %s: %w", att.Filename, err)
			}
		}

		mw.Close()
		return buf.Bytes(), nil
	}

	// For simple single-part messages, write directly
	if req.HTML != "" {
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("Content-Transfer-Encoding", "quoted-printable")
	} else {
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Content-Transfer-Encoding", "quoted-printable")
	}

	// Write headers
	fields := header.Header.Fields()
	for fields.Next() {
		raw, err := fields.Raw()
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
	}
	buf.WriteString("\r\n")

	// Write body (quoted-printable encoded)
	qpWriter := quotedprintable.NewWriter(&buf)
	if req.HTML != "" {
		qpWriter.Write([]byte(req.HTML))
	} else {
		qpWriter.Write([]byte(req.Text))
	}
	qpWriter.Close()

	return buf.Bytes(), nil
}

type partCreator interface {
	CreatePart(header mail.InlineHeader) (*mail.Part, error)
}

// Wrapper for CreateSingleInline relative to CreatePart?
// mail.Writer has CreateSingleInline, InlineWriter has CreatePart.
// Let's simplify and duplicate logic slightly to avoid interface complexity or cast.
func createPart(w interface{}, contentType, content string) error {
	var h mail.InlineHeader
	h.Set("Content-Type", contentType+"; charset=utf-8")

	var p io.WriteCloser
	var err error

	switch writer := w.(type) {
	case *mail.InlineWriter:
		p, err = writer.CreatePart(h)
	case *mail.Writer:
		p, err = writer.CreateSingleInline(h)
	default:
		return fmt.Errorf("unknown writer type")
	}

	if err != nil {
		return err
	}
	if _, err = p.Write([]byte(content)); err != nil {
		p.Close()
		return err
	}
	return p.Close()
}

// createAttachment creates an attachment part with base64 encoding
func createAttachment(w *mail.Writer, att Attachment) error {
	// Decode base64 content
	content, err := base64.StdEncoding.DecodeString(att.Content)
	if err != nil {
		return fmt.Errorf("invalid base64 content: %w", err)
	}

	// Create attachment header
	var h mail.AttachmentHeader
	h.Set("Content-Type", att.ContentType)
	h.SetFilename(att.Filename)

	// Create attachment part
	aw, err := w.CreateAttachment(h)
	if err != nil {
		return err
	}

	// Write decoded content
	if _, err := aw.Write(content); err != nil {
		aw.Close()
		return err
	}

	return aw.Close()
}

// parseHeaderMode extracts request parameters from HTTP headers and body.
// Expected headers:
//   - X-Envelope-From: sender@example.com
//   - X-Envelope-To: recipient@example.com
//   - X-DKIM-Private-Key: <base64 encoded private key> (optional)
//   - X-DKIM-Selector: selector (optional)
//   - X-DKIM-Domain: example.com (optional)
//   - X-ARC-Private-Key: <base64 encoded private key> (optional)
//   - X-ARC-Selector: selector (optional)
//   - X-ARC-Domain: example.com (optional)
//   - X-Inbound-DKIM-Result: pass|fail|none (optional, forwarding only)
//   - X-Inbound-SPF-Result: pass|fail|softfail|none (optional, forwarding only)
//   - X-Inbound-DMARC-Result: pass|fail|none (optional, forwarding only)
//   - Content-Type: message/rfc822
//
// Body should contain the raw RFC822 message.
// Private keys in headers MUST be base64-encoded (newlines not allowed in HTTP headers).
func (h *Handler) parseHeaderMode(r *http.Request) MessageRequest {
	// Read raw message body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error("failed to read request body", "error", err)
		body = []byte{}
	}

	// Decode base64-encoded private keys if present
	dkimKey := h.decodeBase64Header(r.Header.Get("X-DKIM-Private-Key"))
	arcKey := h.decodeBase64Header(r.Header.Get("X-ARC-Private-Key"))

	// Build inbound auth results from headers only if at least one is present
	var inboundAuth *delivery.InboundAuthResults
	dkimRes := r.Header.Get("X-Inbound-DKIM-Result")
	spfRes := r.Header.Get("X-Inbound-SPF-Result")
	dmarcRes := r.Header.Get("X-Inbound-DMARC-Result")
	if dkimRes != "" || spfRes != "" || dmarcRes != "" {
		inboundAuth = &delivery.InboundAuthResults{
			DKIM:  dkimRes,
			SPF:   spfRes,
			DMARC: dmarcRes,
		}
	}

	return MessageRequest{
		From:           r.Header.Get("X-Envelope-From"),
		To:             r.Header.Get("X-Envelope-To"),
		RawMessage:     string(body),
		DKIMPrivateKey: dkimKey,
		DKIMSelector:   r.Header.Get("X-DKIM-Selector"),
		DKIMDomain:     r.Header.Get("X-DKIM-Domain"),
		ARCPrivateKey:  arcKey,
		ARCSelector:    r.Header.Get("X-ARC-Selector"),
		ARCDomain:      r.Header.Get("X-ARC-Domain"),
		InboundAuth:    inboundAuth,
	}
}

// decodeBase64Header attempts to decode a base64-encoded header value.
// If decoding fails or the value is empty, returns the original value.
// This allows both base64-encoded and plain values (for backwards compatibility).
func (h *Handler) decodeBase64Header(value string) string {
	if value == "" {
		return ""
	}

	// Try to decode as base64
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		// Not valid base64, return original value
		// (Could be a plain value or intentionally not encoded)
		return value
	}

	return string(decoded)
}

// validateEmailAddress validates an email address for SMTP delivery.
// It performs structural validation per RFC 5321 to catch malformed addresses
// before they reach the SMTP layer (which would produce cryptic errors).
//
// Accepted forms:
//   - user@domain.tld            (standard)
//   - "quoted local"@domain.tld  (quoted local-part)
//   - <>                         (null sender / bounce, from only)
//
// This is intentionally not a full RFC 5322 parser — it validates the
// minimum structure needed for SMTP MAIL FROM / RCPT TO to succeed.
func validateEmailAddress(addr string, fieldName string, allowNull bool) error {
	// Handle null sender (<>) — valid for bounce/DSN messages (RFC 5321)
	if allowNull && addr == "<>" {
		return nil
	}

	// Strip angle brackets if present: <user@domain> → user@domain
	if len(addr) >= 2 && addr[0] == '<' && addr[len(addr)-1] == '>' {
		addr = addr[1 : len(addr)-1]
	}

	if addr == "" {
		return fmt.Errorf("%s: address is empty", fieldName)
	}

	// Must contain exactly one @
	atIdx := strings.LastIndex(addr, "@")
	if atIdx < 0 {
		return fmt.Errorf("%s: missing '@' separator in %q", fieldName, addr)
	}

	localPart := addr[:atIdx]
	domain := addr[atIdx+1:]

	// Local part validation
	if localPart == "" {
		return fmt.Errorf("%s: empty local part in %q", fieldName, addr)
	}
	if len(localPart) > 64 {
		return fmt.Errorf("%s: local part exceeds 64 characters in %q", fieldName, addr)
	}

	// Domain validation
	if domain == "" {
		return fmt.Errorf("%s: empty domain in %q", fieldName, addr)
	}
	if len(domain) > 255 {
		return fmt.Errorf("%s: domain exceeds 255 characters in %q", fieldName, addr)
	}

	// Domain must contain at least one dot (no bare hostnames)
	// Exception: localhost and IP literals [x.x.x.x] (but we don't support those for delivery)
	if !strings.Contains(domain, ".") {
		return fmt.Errorf("%s: domain %q has no TLD (expected domain.tld format)", fieldName, domain)
	}

	// Domain must not start or end with a dot or hyphen
	if domain[0] == '.' || domain[0] == '-' {
		return fmt.Errorf("%s: domain %q starts with invalid character", fieldName, domain)
	}
	if domain[len(domain)-1] == '.' || domain[len(domain)-1] == '-' {
		return fmt.Errorf("%s: domain %q ends with invalid character", fieldName, domain)
	}

	// Check for consecutive dots in domain
	if strings.Contains(domain, "..") {
		return fmt.Errorf("%s: domain %q contains consecutive dots", fieldName, domain)
	}

	// Domain labels: each part between dots must be 1-63 characters, alphanumeric + hyphen
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) == 0 {
			return fmt.Errorf("%s: domain %q has empty label", fieldName, domain)
		}
		if len(label) > 63 {
			return fmt.Errorf("%s: domain label %q exceeds 63 characters", fieldName, label)
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
				// Allow non-ASCII for internationalized domain names (IDN)
				if c <= 127 {
					return fmt.Errorf("%s: domain %q contains invalid character %q", fieldName, domain, string(c))
				}
			}
		}
	}

	// Total address length check (RFC 5321: max 256 for forward-path)
	if len(addr) > 254 {
		return fmt.Errorf("%s: address exceeds 254 characters", fieldName)
	}

	return nil
}

