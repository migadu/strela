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
	"strings"
	"time"

	"fune/internal/config"
	"fune/internal/delivery"
	"fune/internal/dkim"

	"github.com/emersion/go-message/mail"
)

// Handler manages HTTP API endpoints.
type Handler struct {
	config         *config.Config
	deliveryEngine *delivery.Deliverer
	logger         *slog.Logger
}

// NewHandler creates a new HTTP handler.
func NewHandler(cfg *config.Config, engine *delivery.Deliverer, logger *slog.Logger) *Handler {
	return &Handler{
		config:         cfg,
		deliveryEngine: engine,
		logger:         logger,
	}
}

// MessageRequest represents the JSON request body for message submission.
type MessageRequest struct {
	From           string `json:"from"`
	To             string `json:"to"`
	Subject        string `json:"subject"`
	Text           string `json:"text"`
	HTML           string `json:"html"`
	DKIMPrivateKey string `json:"dkim_private_key,omitempty"`
	DKIMSelector   string `json:"dkim_selector,omitempty"`
	DKIMDomain     string `json:"dkim_domain,omitempty"`
}

// HandleDeliver handles synchronous message delivery requests.
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Authentication
	if h.config.Inbound.AuthToken != "" {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.config.Inbound.AuthToken)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// 2. Parse request
	var req MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// 3. Validation
	if req.From == "" || req.To == "" {
		http.Error(w, "Missing 'from' or 'to' fields", http.StatusBadRequest)
		return
	}

	// 4. Build MIME message
	rawMessage, err := h.buildRawMessage(&req)
	if err != nil {
		h.logger.Error("failed to build MIME message", "error", err)
		http.Error(w, "Failed to build message", http.StatusInternalServerError)
		return
	}

	// 5. Create context with timeout
	timeout := time.Duration(h.config.Outbound.DeliveryTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// 6. Synchronous Delivery
	result := h.deliveryEngine.DeliverMessage(ctx, req.From, req.To, rawMessage)

	// 7. Map result to HTTP status
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
