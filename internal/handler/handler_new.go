package handler

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/dkim"

	"github.com/emersion/go-message/mail"
)

// DeliveryEngine defines the interface for message delivery
type DeliveryEngine interface {
	DeliverMessage(ctx context.Context, from, to string, message []byte, dkimPrivateKey, dkimSelector, dkimDomain string, skipDKIMValidation bool, arcPrivateKey, arcSelector, arcDomain string) delivery.DeliveryResult
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

// MessageRequest represents the JSON request body for message submission.
// Supports two modes:
// 1. Composed mode: Provide from, to, subject, text/html (Fune builds MIME message)
// 2. Raw mode: Provide from, to, raw_message (Fune forwards pre-built RFC822 message)
type MessageRequest struct {
	From               string `json:"from"`
	To                 string `json:"to"`
	Subject            string `json:"subject,omitempty"`              // Required for composed mode, ignored for raw mode
	Text               string `json:"text,omitempty"`                 // Optional for composed mode, ignored for raw mode
	HTML               string `json:"html,omitempty"`                 // Optional for composed mode, ignored for raw mode
	MessageID          string `json:"message_id,omitempty"`           // Optional Message-ID for composed mode; auto-generated if not provided
	RawMessage         string `json:"raw_message,omitempty"`          // Raw RFC822 message (forwarding mode) - mutually exclusive with subject/text/html
	DKIMPrivateKey     string `json:"dkim_private_key,omitempty"`     // Override config DKIM key
	DKIMSelector       string `json:"dkim_selector,omitempty"`        // Override config DKIM selector
	DKIMDomain         string `json:"dkim_domain,omitempty"`          // Override config DKIM domain
	SkipDKIMValidation bool   `json:"skip_dkim_validation,omitempty"` // Skip DNS validation (faster but less safe)
	ARCPrivateKey      string `json:"arc_private_key,omitempty"`      // Override config ARC key (for dynamic/multi-tenant scenarios)
	ARCSelector        string `json:"arc_selector,omitempty"`         // Override config ARC selector
	ARCDomain          string `json:"arc_domain,omitempty"`           // Override config ARC domain
}

// HandleDeliver handles synchronous message delivery requests.
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("received delivery request",
		"remote_addr", r.RemoteAddr,
		"method", r.Method,
		"url", r.URL.String())

	if r.Method != http.MethodPost {
		h.logger.Debug("method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authentication
	if h.config.Inbound.AuthToken != "" {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.config.Inbound.AuthToken)) != 1 {
			h.logger.Debug("authentication failed", "remote_addr", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.logger.Debug("authentication successful", "remote_addr", r.RemoteAddr)
	}

	// 2. Parse request
	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Debug("failed to decode JSON payload", "error", err, "remote_addr", r.RemoteAddr)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	h.logger.Debug("parsed message request",
		"from", req.From,
		"to", req.To,
		"subject", req.Subject,
		"has_dkim_key", req.DKIMPrivateKey != "")

	// 3. Validation
	if req.From == "" || req.To == "" {
		h.logger.Debug("missing required fields", "from", req.From, "to", req.To)
		http.Error(w, "Missing 'from' or 'to' fields", http.StatusBadRequest)
		return
	}

	// Determine mode: raw message or composed
	isRawMode := req.RawMessage != ""
	isComposedMode := req.Subject != "" || req.Text != "" || req.HTML != ""

	if isRawMode && isComposedMode {
		h.logger.Debug("conflicting fields: raw_message and composed fields both provided")
		http.Error(w, "Cannot use both 'raw_message' and composed fields (subject/text/html)", http.StatusBadRequest)
		return
	}

	if !isRawMode && !isComposedMode {
		h.logger.Debug("missing message content")
		http.Error(w, "Must provide either 'raw_message' or at least one of 'subject'/'text'/'html'", http.StatusBadRequest)
		return
	}

	// 4. Prepare message payload
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

	// 5. Apply DKIM config (respects enabled flag)
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

	// 6. Create context with timeout
	timeout := time.Duration(h.config.Outbound.DeliveryTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// 7. Synchronous Delivery
	h.logger.Debug("starting synchronous delivery", "from", req.From, "to", req.To)
	result := h.deliveryEngine.DeliverMessage(ctx, req.From, req.To, rawMessage, dkimPrivateKey, dkimSelector, dkimDomain, skipDKIMValidation, arcPrivateKey, arcSelector, arcDomain)
	h.logger.Debug("delivery attempt finished",
		"status", result.Status,
		"mx", result.MXHost,
		"source_ip", result.SourceIP,
		"duration_ms", result.AttemptDurationMs)

	// 8. Map result to HTTP status
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
		return http.StatusOK
	case "temp_fail":
		return http.StatusUnprocessableEntity // 422 - usually means retry later
	case "rate_limit":
		return http.StatusTooManyRequests // 429 - retry later (Fail Fast)
	case "hard_bounce":
		return http.StatusBadRequest // 400 - permanent failure
	case "timeout":
		return http.StatusGatewayTimeout // 504
	case "error":
		return http.StatusInternalServerError // 500
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

	mw, err := mail.CreateWriter(&buf, header)
	if err != nil {
		return nil, err
	}

	if req.HTML != "" && req.Text != "" {
		iw, err := mw.CreateInline()
		if err != nil {
			return nil, err
		}
		createPart(iw, "text/plain", req.Text)
		createPart(iw, "text/html", req.HTML)
		iw.Close()
	} else if req.HTML != "" {
		createPart(mw, "text/html", req.HTML)
	} else {
		createPart(mw, "text/plain", req.Text)
	}

	mw.Close()
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

// Helper methods for validation could go here
func (h *Handler) validateDKIM(req *MessageRequest) error {
	if req.DKIMPrivateKey != "" {
		_, err := dkim.ValidatePrivateKey(req.DKIMPrivateKey)
		return err
	}
	return nil
}
